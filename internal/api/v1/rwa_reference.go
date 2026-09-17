package v1

import (
	"context"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Oracle NAV reference and premium/discount for /v1/rwa/assets.
//
// A tokenized treasury has two prices: what an independent oracle says
// the instrument is worth, and what the Stellar market will pay for the
// token. The gap between them is the number a holder of a tokenized
// treasury actually needs, and it is the one figure on this surface that
// no query over public chain data alone can produce — it needs both the
// oracle stream and the gated market price, which this index holds
// together.
//
// Five rules decide whether the gap may be published. Each removes a way
// of publishing a number that means something other than what it says.
//
// R-0 — THE REFERENCE MUST BE BOUND TO THIS (CODE, ISSUER). Asset codes
// are not unique on Stellar; any account may issue a token called USTRY.
// A reference joined on the code alone answers every one of them with the
// real instrument's net asset value, and an unrelated token trading at
// $0.20 is published at an 81% discount to a security it has nothing to
// do with. [rwa.InstrumentFeed] is a curated, exact, fail-closed binding
// on the pair (the ADR-0040 curated-set mechanism); an unbound pair gets
// silence and a stated reason.
//
// R-A — THE REFERENCE MUST BE DOLLARS. A feed's value is denominated in
// whatever the registry says it is denominated in, and a bare
// `_FUNDAMENTAL` feed publishes net asset value in the token's RESERVE
// asset — a RATIO, not a dollar figure. Registering two such feeds as
// USD once served "a BTC-backed token is worth $1.00" for a token its
// own USD sibling priced at $78,313 (D8, internal/sources/redstone
// feeds.go). So the quote is read off the STORED ROW and must be
// `fiat:USD`; a row quoted in anything else is refused with that as the
// stated reason, never unit-converted into dollars here.
//
// R-B — THE REFERENCE MUST PRICE A TOKEN. `rwa:XAU` is spot gold per
// troy ounce and `rwa:SPXU` is one share of an exchange-traded fund;
// neither is one token of anything. No binding may target one, and a
// token of such a code is refused with that as the stated reason.
//
// R-C — THE PUBLISHER MUST BE AN ORACLE. Only sources the registry
// classes [external.ClassOracle] qualify. Aggregators and the
// authority-sanity feeds write into the same hypertable for divergence
// comparison; a premium measured against an aggregator's own guess at
// the market price would be the market compared with itself.
//
// R-D — THE MARKET PRICE MUST BE OBSERVED. A price carried on
// `price_basis` is a declared peg or a transitive derivation rather than
// a market's opinion. Measuring a premium against a declared peg reports
// the issuer's own claim back as a market finding.
//
// A row failing any of them carries no premium figure and says which
// rule refused it. None of them ever produces a zero.
//
// # The reference-priced valuation
//
// The same reference, multiplied by the float, is `reference_valuation`
// — what the backing behind the circulating tokens is claimed to be
// worth. It exists because a real-world asset is bought and held rather
// than traded: most of the set has never had a trade on this network,
// so a market-price valuation has nothing to work with and publishes
// nothing at all, while the instrument behind it is priced daily by an
// oracle this index already reads.
//
// It rides R-0 through R-C — an unbound pair, a non-dollar quote, an
// off-chain unit, a non-oracle publisher and a flagged issuer all
// refuse it exactly as they refuse a reference — and it is deliberately
// NOT subject to R-D, the rule requiring an observed market price:
// there is no market price in it. That is also its whole limitation,
// and the reason it is published under its own name, with its own
// total, beside `market_cap_usd` and never inside it. Nothing about
// this figure changes what a market cap means or which rows carry one.
//
// # One refusal, two fields
//
// Every rule above refuses the premium and the reference valuation
// together, so `premium.status` and `reference_valuation.status` carry
// the SAME string on those rows — assigned at the same line, from the
// same constant, so they cannot drift into two accounts of one event.
// They diverge only where the reasons genuinely differ: a row with a
// reference but no observed market price has no premium and a full
// reference valuation, and a row with a reference but no supply reading
// has the premium and no valuation.

// rwaReferenceTTL bounds the reuse of one oracle-stream snapshot.
//
// The RWA feeds publish on a cadence of seconds to minutes, so 30s keeps
// the surface off a per-request hypertable scan while never showing a
// figure materially older than an uncached read would have. Deliberately
// far shorter than [rwaMembershipTTL]: membership moves daily, a price
// does not.
const rwaReferenceTTL = 30 * time.Second

// rwaReferenceStaleAfter is the age past which a reference is served
// with `stale: true`.
//
// 72 hours is the longest ordinary gap between two strikes of a
// real-world instrument's value: a Friday figure read on the following
// Monday, plus one public holiday. Shorter would flag every weekend on a
// treasury fund, whose value is struck on business days by construction.
//
// It LABELS, it does not withhold. A NAV struck last Friday is the
// current NAV on Monday morning, and suppressing it would hide the only
// independent valuation the surface has. The absolute outer bound is
// [rwaReferenceMaxAge].
const rwaReferenceStaleAfter = 72 * time.Hour

// rwaReferenceMaxAge is the age past which an observation is not served
// at all.
//
// Seven days is not a threshold chosen here: it is the window
// LatestOracleStreams defines an ACTIVE stream by, so a live read can
// never return anything older. The check exists because a snapshot is
// carried forward across a failed read, and a sustained outage would
// otherwise let the carried rows age past the bound this file and
// docs/methodology/rwa-definition.md both claim is absolute.
const rwaReferenceMaxAge = 7 * 24 * time.Hour

// ─── wire shape ─────────────────────────────────────────────────────

// RWAReference is an independent oracle's valuation of the real-world
// instrument an admitted token declares it anchors to.
//
// It is NOT this platform's price for the token, and it is not derived
// from any Stellar market. Both figures are served so a reader can see
// the two independently rather than only their ratio.
type RWAReference struct {
	// PriceUSD is the oracle's published value, verbatim at the feed's
	// own decimal scale (decimal string — ADR-0003). Not re-rounded: the
	// scale is the oracle's statement of its own precision.
	PriceUSD string `json:"price_usd"`
	// Source is the registered oracle that published it.
	Source string `json:"source"`
	// Feed is the canonical asset id of the instrument the oracle
	// priced — `rwa:<CODE>`. Present so a reader can pull the same row
	// from /v1/oracle/latest and check this figure against its origin.
	Feed string `json:"feed"`
	// Quote is the denominator, always `fiat:USD` on a served row. It is
	// on the wire rather than assumed because a NAV feed denominated in
	// a reserve asset is a ratio, and the difference is invisible in the
	// number alone.
	Quote string `json:"quote"`
	// AsOf is when the oracle published it. The market price beside it
	// carries the response's own as_of, so the two vintages are
	// separately visible.
	AsOf WireTime `json:"as_of"`
	// Stale marks a reference older than 72h — served, but labelled.
	Stale bool `json:"stale,omitempty"`
	// Provenance names WHAT KIND of figure this is. Two are published
	// and they are not the same claim, so the field is mandatory on
	// every served reference rather than defaulted.
	//
	// [RWAReferenceOracleNAV] is an oracle's published valuation of the
	// INSTRUMENT, which the token declares it anchors to one-for-one.
	// [RWAReferenceListingPrice] is a listing platform's aggregate of
	// what the TOKEN trades at across the venues it tracks. The first
	// is a statement about the backing; the second is a statement about
	// the token. Folding them under one name would let a summary total
	// describe itself with the wrong provenance, which is the exact
	// defect the summary basis prose exists to prevent.
	Provenance string `json:"provenance"`
}

// RWAReference provenances.
const (
	// RWAReferenceOracleNAV — an ADR-0028 oracle feed's published value
	// for the real-world instrument, joined through the curated
	// (code, issuer) binding. The original and stronger arm: the
	// publisher is a registered oracle, the subject is the instrument,
	// and the correspondence to one token is the issuer's own
	// domain-bound declaration.
	RWAReferenceOracleNAV = "oracle_instrument_nav"
	// RWAReferenceListingPrice — the independent listing directory's
	// own USD price for the token, from the same source that
	// corroborated the address at C2.
	//
	// Weaker than the oracle arm in one specific and stated way: it
	// prices the TOKEN, not the instrument. It therefore carries no
	// claim that one token is one unit of anything, and it cannot
	// produce a premium, because a premium against an aggregate of the
	// markets is the market compared with itself.
	//
	// Available to any contract row the listing directory NAMES,
	// whichever C2 arm admitted it. That is deliberately not the same
	// set as "admitted on [rwa.RecognitionListingCorroborated]": four
	// addresses are named by both sources, and one of those admitted by
	// the curated directory on its own is still an address an
	// independent listing bound a price to. Refusing it would withhold
	// a figure this surface can correctly make.
	//
	// What the listing entry supplies is the same in both cases — a USD
	// price bound to a 56-character address by a party that did not
	// read our directory — so the prose describes THAT rather than the
	// requirement the row happened to satisfy. A contract the listing
	// never named has no listing price to serve, which is the same fact
	// that refuses it at C2 when nothing else names it either.
	RWAReferenceListingPrice = "listing_platform_price"
	// RWAReferenceProspectusCNAV — the issuer's published NAV for a
	// share class whose fund rules fix it (a CNAV money market fund),
	// bound on the exact (code, issuer) in rwa.ConstantNAV. Weaker than
	// the oracle arm — nobody independent measured it — and stronger
	// than nothing in the one way that matters: the value is prescribed
	// by an authorised prospectus and published daily by the issuer.
	// Taken only when neither an oracle binding nor a listing price
	// exists for the row.
	RWAReferenceProspectusCNAV = "prospectus_constant_nav"
)

// RWAReferenceValuation is the token's circulating supply valued at the
// REFERENCE price — the oracle's published value of the instrument —
// rather than at anything the market was observed paying.
//
// It is the deliberate opposite of `valuation.market_cap_usd`, and the
// two are never folded together:
//
//   - `valuation.market_cap_usd` is adversarially verified. A price
//     reaches it only after the thin-market substance gate, the
//     dust-liquidity guard and the scam-issuer suppression have all
//     declined to withhold it, and behind that price is a trade
//     somebody actually settled. It is an observation.
//   - this figure is a CLAIM. The oracle states what the backing is
//     worth and the issuer's own domain-bound declaration states that
//     one token is one unit of that backing; multiplying the second by
//     the first values the float. Nothing here was paid by anyone, no
//     liquidity gate can measure it, and a token with no market at all
//     carries this figure at full size. That is the point of it — a
//     real-world asset is bought and held, so most of the set never
//     trades — and it is also exactly why it may not be reported as a
//     market capitalisation.
//
// The distinction has to survive being read carelessly, so it is
// carried three ways: the field is named for the reference rather than
// for the market, every published figure names the feed and the vintage
// that produced it in the sibling `reference` block, and
// `summary.reference_valuation.basis` states in prose that nobody was
// observed paying it.
type RWAReferenceValuation struct {
	// Status is the single authority on why there is no figure. Where a
	// rule refuses the reference itself it carries the SAME string as
	// `premium.status`, assigned together — so a reader, and the funnel,
	// need consult exactly one field.
	Status string `json:"status"`
	// ValueUSD is circulating supply (scaled by the asset's own
	// decimals) times the reference price, as a 2-dp decimal string —
	// ADR-0003, exact rational arithmetic throughout, never a float.
	// Present if and only if Status is published.
	ValueUSD *string `json:"value_usd,omitempty"`
}

// RWAReferenceValuation statuses that are NOT shared with the premium.
// Every other value it can carry is one of the RWAPremium constants
// below, because the rule that refused the premium refused this too.
const (
	// RWAReferenceValuationPublished — a reference price and a supply
	// reading were both available.
	RWAReferenceValuationPublished = "published"
	// RWAReferenceValuationDecimalsUnknown — a reference and a supply
	// both exist and the token's own declared SCALE does not, so there
	// is no exponent to divide the float by.
	//
	// The same refusal [RWAValuationDecimalsUnknown] makes on the market
	// basis, for the same reason and with more at stake: this basis
	// values tokens that never trade, so it is the figure most likely to
	// be the only number on the row. Defaulting the exponent would
	// publish a dollar total wrong by a factor of ten to the something,
	// silently, under `status: published`.
	RWAReferenceValuationDecimalsUnknown = "decimals_unavailable"
	// RWAReferenceValuationNoSupply — a reference exists but no
	// circulating-supply reading does, so there is no float to value. A
	// non-numeric or negative reading is treated the same way: it is
	// not a supply. The premium is unaffected by it — a premium
	// compares two prices and needs no supply at all — which is why
	// this status is the valuation's own.
	RWAReferenceValuationNoSupply = "supply_unavailable"
)

// RWAPremium is the token's market price measured against the oracle's
// valuation of the instrument, or the reason there is no such figure.
type RWAPremium struct {
	Status string `json:"status"`
	// Pct is (market − reference) / reference × 100 as a decimal string:
	// positive when the token trades ABOVE the instrument's independent
	// valuation, negative when below. Present only when Status is
	// published. Never zero-filled — a status other than published
	// means the comparison could not be made, which is a different
	// statement from "it trades at par".
	Pct *string `json:"pct,omitempty"`
}

// RWAPremium status values.
const (
	// RWAPremiumPublished — both figures exist and are comparable.
	RWAPremiumPublished = "published"
	// RWAPremiumIssuerFlagged — the issuer carries a scam-class
	// directory tag. No valuation of any kind is published for it,
	// including a third party's: an impersonator handed a real
	// instrument's NAV is exactly the claim the flag exists to deny.
	RWAPremiumIssuerFlagged = "withheld_issuer_flagged"
	// RWAPremiumContractNotBound — the member is CONTRACT-issued, and
	// nothing binds a contract address to an oracle feed.
	//
	// Reported apart from [RWAPremiumNotBound] because that status
	// names a (code, issuer) pair, which a contract row does not have,
	// and because the two are refusals of different shapes. A classic
	// pair is usually unbound because it is a code collision; a
	// contract is unbound because no contract-to-feed binding set
	// exists at all.
	//
	// The obvious join is available and is REFUSED. A contract admitted
	// on [rwa.BasisContractOracleFeed] got in because its on-chain
	// SEP-41 symbol is an ADR-0028 code — but a symbol is metadata the
	// contract itself authors, so pricing a token by it is the
	// code-keyed join this file exists to refuse, with a weaker key.
	// Recognition of the address establishes WHO deployed it; it does
	// not establish that one of its tokens is one unit of the
	// instrument an oracle prices under that name, which is the claim a
	// reference valuation makes. The curated contract set
	// ([rwa.ContractInstrumentBindings]) records instrument and class,
	// NOT a feed, so it cannot answer this either however many entries
	// it holds — a fund's identity is not a price for it. Pricing a
	// curated contract would need a contract-to-feed binding set that
	// does not exist, and for the funds currently bound there is no
	// oracle feed to bind to.
	RWAPremiumContractNotBound = "reference_contract_not_bound"
	// RWAPremiumNotBound — no curated binding ties this exact
	// (code, issuer) to an oracle feed. The commonest cause by far is
	// that the pair is a code collision: a token wearing an instrument's
	// ticker that the oracle has never priced. It also covers a genuine
	// issuer nobody has bound yet, which is why the refusal is reported
	// rather than absorbed.
	RWAPremiumNotBound = "reference_not_bound"
	// RWAPremiumNoReference — the pair IS bound, but the oracle stream
	// carries no row for its feed. Means exactly that and nothing else.
	RWAPremiumNoReference = "no_reference_feed"
	// RWAPremiumReferenceUnavailable — the oracle read did not answer, so
	// nothing is known either way. Distinct from every refusal above: a
	// read that failed is not entitled to report an absence as a finding,
	// and on the wire it must not look like one.
	RWAPremiumReferenceUnavailable = "reference_unavailable"
	// RWAPremiumReferenceExpired — the bound feed's most recent
	// observation is older than the 7-day window an active stream is
	// defined by. Reached only when a snapshot is carried forward across
	// a sustained read failure; a live read cannot return such a row.
	RWAPremiumReferenceExpired = "reference_expired"
	// RWAPremiumNotInstrumentScoped — a feed of this code exists but
	// prices an off-chain quantity in its own unit (a troy ounce of spot
	// metal, one fund share) rather than one token. Its ratio to a token
	// price would be a unit conversion wearing a premium's clothes.
	RWAPremiumNotInstrumentScoped = "reference_not_instrument_scoped"
	// RWAPremiumReferenceNotUSD — a feed exists but its stored quote is
	// not fiat:USD, so its value is a ratio in a reserve asset and is
	// not a dollar figure (D8). Not converted here.
	RWAPremiumReferenceNotUSD = "reference_not_usd_denominated"
	// RWAPremiumNoMarketPrice — a reference exists but no served USD
	// market price does, so there is nothing to compare it against. The
	// reference itself is still published.
	RWAPremiumNoMarketPrice = "no_market_price"
	// RWAPremiumMarketNotObserved — the served price is a declared peg
	// or a transitive derivation, not a market observation. A premium
	// against it would report the issuer's own claim as a market
	// finding.
	RWAPremiumMarketNotObserved = "market_price_not_observed"
	// RWAPremiumReferenceNotPositive — the oracle published a
	// non-positive value. Nothing is divided by it.
	RWAPremiumReferenceNotPositive = "reference_not_positive"
	// RWAPremiumReferenceNotOracle — the row carries a reference, and
	// it is a LISTING PRICE rather than an oracle's valuation of the
	// instrument, so no premium may be computed against it.
	//
	// This is R-C applied to the new arm, and it is the one status on
	// this list that refuses the premium while the reference valuation
	// beside it is PUBLISHED. A premium is the gap between what the
	// market pays and what the backing is independently worth. A
	// listing price is an aggregate of the same markets our own price
	// comes from, so the gap between them measures the disagreement
	// between two samples of one market — not a premium to anything,
	// and it would be published under a name that says it is.
	//
	// The obvious arithmetic is available and is refused, for the same
	// reason [RWAPremiumContractNotBound] refuses the symbol join: a
	// number that can be computed is not thereby a number that means
	// something.
	RWAPremiumReferenceNotOracle = "reference_is_a_listing_price"
)

// ─── snapshot ───────────────────────────────────────────────────────

// rwaReference is one admissible reference, reduced from an oracle row.
type rwaReference struct {
	priceUSD *big.Rat
	// wire is the published decimal string at the feed's own scale.
	wire   string
	source string
	feed   string
	asOf   time.Time
}

// rwaReferences is one oracle-stream snapshot reduced to what this
// surface may publish, keyed by the ADR-0028 FEED code — exactly as the
// allow-list spells it, never folded. The join from a Stellar asset to a
// feed happens through [rwa.InstrumentFeed]; this map is only the
// feed-side half of it.
type rwaReferences struct {
	// byFeed holds the references that passed R-A and R-C.
	byFeed map[string]rwaReference
	// nonUSD records the feeds whose only oracle rows are denominated in
	// something other than dollars, so a row can state THAT as the
	// reason rather than the weaker "no feed". Keyed the same way; the
	// value is the quote's canonical id.
	nonUSD map[string]string
	// available is false when no oracle reader is wired or the read
	// failed. Distinguished from "the oracles publish nothing for these
	// instruments", which is a finding a failed read may not make.
	available bool
}

// rwaReferenceSnapshot reduces one oracle-stream read to the references
// this surface may publish.
//
// It runs over the SAME rows /v1/oracle/streams serves — one per
// (source, asset, quote) in the trailing 7d — rather than a per-code
// query, for two reasons. One read serves the whole set, so the surface
// costs the lake a single scan however many members it has. And the 7d
// window is then not a threshold invented here: a feed absent from that
// window is not an active stream, and the row correctly carries no
// reference instead of a figure from a source that has gone quiet.
func rwaReferenceSnapshotFrom(updates []canonical.OracleUpdate) rwaReferences {
	out := rwaReferences{
		byFeed:    map[string]rwaReference{},
		nonUSD:    map[string]string{},
		available: true,
	}
	for _, u := range updates {
		// Namespace gate: only ADR-0028 instrument feeds. Whether any
		// Stellar asset may be answered with one is R-0's question, asked
		// per row against the curated binding — not here.
		if u.Asset.Type != canonical.AssetRWA {
			continue
		}
		// R-C — an oracle, not an aggregator writing into the same table
		// for divergence comparison.
		if external.Lookup(u.Source).Class != external.ClassOracle {
			continue
		}
		// The key is the stored feed code verbatim. canonical.ParseAsset
		// only admits the ADR-0028 spelling, so it is already exact; no
		// folding is applied here or at lookup.
		key := u.Asset.Code
		// R-A — dollars, read off the stored row.
		if !isUSDQuote(u.Quote) {
			// Keep only the first, so the reason is stable when a code
			// has several non-USD legs.
			if _, seen := out.nonUSD[key]; !seen {
				out.nonUSD[key] = u.Quote.String()
			}
			continue
		}
		ref := rwaReference{
			priceUSD: ratFromScaledInt(u.Price.BigInt(), u.Decimals),
			wire:     scaledDecimalString(u.Price.BigInt(), u.Decimals),
			source:   u.Source,
			feed:     u.Asset.String(),
			asOf:     u.Timestamp,
		}
		// Several oracles may price the same instrument. Take the most
		// recent, and break an exact tie on source name so the served
		// figure does not depend on the order the scan returned rows.
		if prev, ok := out.byFeed[key]; ok {
			if prev.asOf.After(ref.asOf) ||
				(prev.asOf.Equal(ref.asOf) && prev.source <= ref.source) {
				continue
			}
		}
		out.byFeed[key] = ref
	}
	// A feed whose USD leg was admitted is not missing a reference, so
	// drop any non-USD note for it: the reason must describe the row
	// that would be served, not one that was passed over.
	for feed := range out.byFeed {
		delete(out.nonUSD, feed)
	}
	return out
}

// isUSDQuote reports whether an oracle row's denominator is the dollar.
// Exact on the canonical fiat asset — never a code comparison, because
// `crypto:USDC` and `fiat:USD` are different denominators and the
// difference is what R-A is about.
func isUSDQuote(q canonical.Asset) bool {
	return q.Type == canonical.AssetFiat && q.Code == "USD"
}

// ratFromScaledInt turns a fixed-point oracle value into an exact
// rational (ADR-0003 — never a float on a money path).
func ratFromScaledInt(value *big.Int, decimals uint8) *big.Rat {
	if value == nil {
		return nil
	}
	r := new(big.Rat).SetInt(value)
	if decimals == 0 {
		return r
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return r.Quo(r, new(big.Rat).SetInt(scale))
}

// cachedRWAReferences returns the reference snapshot, refreshed at most
// once per TTL window and shared across concurrent requests by a
// single-flight gate — the same shape [Server.cachedRWAMembership] uses.
//
// On a failed read it serves the last good snapshot when there is one,
// and an UNAVAILABLE snapshot when there is not. Unavailable is not the
// same as empty: an empty snapshot would tell every row "no oracle
// publishes your instrument", which is a finding, and a read that did
// not answer is not entitled to make it.
func (s *Server) cachedRWAReferences(ctx context.Context) rwaReferences {
	if s.oracle == nil {
		return rwaReferences{}
	}
	s.rwaRefMu.Lock()
	if s.rwaRefCache != nil && time.Since(s.rwaRefAt) < rwaReferenceTTL {
		snap := *s.rwaRefCache
		s.rwaRefMu.Unlock()
		return snap
	}
	if ch := s.rwaRefFlight; ch != nil {
		s.rwaRefMu.Unlock()
		select {
		case <-ch:
			s.rwaRefMu.Lock()
			var snap rwaReferences
			if s.rwaRefCache != nil {
				snap = *s.rwaRefCache
			}
			s.rwaRefMu.Unlock()
			return snap
		case <-ctx.Done():
			return rwaReferences{}
		}
	}
	done := make(chan struct{})
	s.rwaRefFlight = done
	s.rwaRefMu.Unlock()

	var built rwaReferences
	updates, err := s.oracle.LatestOracleStreams(ctx)
	if err != nil {
		s.logger.Warn("rwa references: oracle stream read failed", "err", err)
	} else {
		built = rwaReferenceSnapshotFrom(updates)
	}

	s.rwaRefMu.Lock()
	if built.available {
		s.rwaRefCache = &built
		s.rwaRefAt = time.Now()
	} else if s.rwaRefCache != nil {
		built = *s.rwaRefCache
	}
	s.rwaRefFlight = nil
	s.rwaRefMu.Unlock()
	close(done)
	return built
}

// ─── per-row application ────────────────────────────────────────────

// rwaApplyReference attaches the oracle reference and the premium to one
// served row, or the reason neither is there.
//
// The order of the refusals is the order of the rules, and it is
// load-bearing for the reason reported: a flagged issuer is answered as
// flagged before anything else is consulted, because that refusal is
// about who is asking rather than about what the data holds.
func rwaApplyReference(
	a *RWAAsset,
	snap rwaReferences,
	listings map[string]timescale.ListingEntry,
	classicListings map[string]timescale.ListingEntry,
	now time.Time,
) {
	// The scam-flag suppression comes first and covers the reference
	// too. /v1/assets withholds this issuer's own price; handing it a
	// real instrument's independent NAV instead would publish a larger
	// claim through the gap — a dollar figure on an impersonator, from a
	// source that never named it.
	if a.Valuation.Status == RWAValuationIssuerFlagged {
		rwaRefuseReference(a, RWAPremiumIssuerFlagged)
		return
	}
	// A contract-issued member, answered BEFORE the oracle snapshot is
	// consulted. It never falls through to the (code, issuer) binding
	// below, which would answer with a reason naming an identity this
	// row does not have — and it must not be answered by the oracle
	// read's availability either, because a contract row's reference
	// does not come from the oracle stream at any point. Reporting an
	// oracle outage on a row the oracle could never have priced would
	// withhold a figure for a reason that has nothing to do with it.
	if a.ContractID != "" {
		rwaApplyContractReference(a, listings[a.ContractID], now)
		return
	}
	// A read that did not answer knows nothing either way. Reporting it
	// as an absence would publish a finding the read did not earn, and on
	// the wire it would be indistinguishable from a genuine one.
	if !snap.available {
		rwaRefuseReference(a, RWAPremiumReferenceUnavailable)
		return
	}

	// R-0 — the binding, on the exact (code, issuer). This is the join,
	// and it is the whole reason a token that merely shares a ticker with
	// a real instrument cannot be handed that instrument's valuation.
	feed, bound := rwa.InstrumentFeed(a.Code, a.Issuer)
	if !bound {
		// Both refusals withhold. They differ in what they tell a reader,
		// and the more specific one is worth reporting: a token coded XAU
		// is not unbound by oversight — the oracle of that name prices a
		// troy ounce of metal, which is not a quantity any token has.
		notFound := RWAPremiumNotBound
		if rwa.OffChainReferenceCode(a.Code) {
			notFound = RWAPremiumNotInstrumentScoped
		}
		// Neither an oracle nor the listing directory prices this pair,
		// but its prospectus may: a CNAV share class bound in
		// rwa.ConstantNAV is valued at the NAV its fund rules fix, under
		// its own provenance. A listing price, when one exists, still
		// wins — it is an observation, this is a rule.
		if entry := classicListings[a.AssetID]; entry.PriceUSD == "" {
			if cnav, ok := rwa.ConstantNAV(a.Code, a.Issuer); ok {
				rwaApplyConstantNAVReference(a, cnav, now)
				return
			}
		}
		// Nothing binds this pair to an ORACLE. That is a statement
		// about one source, and the independent listing directory is
		// another — the same directory, the same bound, the same
		// address-exact key that already prices the contract arm.
		//
		// Reading it here is not a new kind of evidence, it is the
		// SAME evidence reaching a row the surface was not offering it
		// to. Measured 2026-09-16: the directory held 33 recognised
		// classic rows, every one of them priced, against 17 contract
		// rows — so two-thirds of it was being read by nothing while
		// this arm reported rows as unpriced beside it.
		//
		// The key is the asset's own `CODE-GISSUER`, never the code:
		// this network carries twenty-six assets coded BENJI and one of
		// them is Franklin Templeton's. A code-keyed lookup here would
		// be the attacker-authored-pricing class in a new coordinate,
		// which is the same reason [rwa.InstrumentFeed] is keyed on the
		// pair above.
		rwaApplyListingReference(a, classicListings[a.AssetID], notFound, now)
		return
	}

	ref, ok := snap.byFeed[feed]
	if !ok {
		if snap.nonUSD[feed] != "" {
			rwaRefuseReference(a, RWAPremiumReferenceNotUSD)
			return
		}
		rwaRefuseReference(a, RWAPremiumNoReference)
		return
	}

	// A live read cannot return an observation older than the 7-day
	// window, but a snapshot carried forward across a sustained read
	// failure can age past it. Enforce the bound on the OBSERVATION
	// rather than on the snapshot, so the documented claim — a feed
	// silent for seven days leaves the row with no reference — holds
	// however the row reached us.
	if now.Sub(ref.asOf) > rwaReferenceMaxAge {
		rwaRefuseReference(a, RWAPremiumReferenceExpired)
		return
	}

	a.Reference = &RWAReference{
		PriceUSD:   ref.wire,
		Source:     ref.source,
		Feed:       ref.feed,
		Quote:      "fiat:USD",
		AsOf:       WireTime(ref.asOf),
		Stale:      now.Sub(ref.asOf) > rwaReferenceStaleAfter,
		Provenance: RWAReferenceOracleNAV,
	}
	// Only now, with the reference attached and its provenance on the
	// row beside it. A supply-valued figure whose feed and vintage were
	// not published would be a dollar total with no traceable source,
	// which on this surface is worse than no figure at all.
	a.ReferenceValuation = rwaReferenceValuationOf(a, ref)

	market := ratFromOptionalString(a.Valuation.PriceUSD)
	switch {
	case market == nil:
		a.Premium = RWAPremium{Status: RWAPremiumNoMarketPrice}
	case a.Valuation.PriceBasis != "":
		// R-D. The price is served — it is on the row above — but it is
		// the issuer's declared peg or a hop through another market, and
		// a premium measured against either is circular.
		a.Premium = RWAPremium{Status: RWAPremiumMarketNotObserved}
	case ref.priceUSD == nil || ref.priceUSD.Sign() <= 0:
		a.Premium = RWAPremium{Status: RWAPremiumReferenceNotPositive}
	default:
		pct := new(big.Rat).Sub(market, ref.priceUSD)
		pct.Quo(pct, ref.priceUSD)
		pct.Mul(pct, big.NewRat(100, 1))
		s := pct.FloatString(4)
		a.Premium = RWAPremium{Status: RWAPremiumPublished, Pct: &s}
	}
}

// rwaApplyContractReference attaches the LISTING-priced reference to a
// contract member, or the reason there is none.
//
// # Why a contract row can carry a reference at all now
//
// It could not before, and the reason was sound and is unchanged:
// nothing binds a contract ADDRESS to an oracle feed, and the available
// joins — the contract's own SEP-41 symbol, or the curated binding's
// instrument name — are both code-keyed joins onto a feed, which is
// exactly what R-0 exists to refuse. The curated set records an
// instrument and a class, not a feed, so it cannot answer a price
// however many entries it holds.
//
// What changed is not that join. It is that this surface now holds, for
// some contract addresses, a row from an independent listing directory
// that NAMES the exact address and publishes a USD price for the thing
// it named — price bound to address, in one row, by a party that did
// not read our curated directory. No code is matched anywhere on this
// path. A token wearing a bound instrument's symbol gets nothing here,
// because it was never in the listing row.
//
// It applies to any contract row the listing names, not only to rows
// the listing ADMITTED. Those two sets overlap but are not equal: a
// contract the curated directory attested on its own may also be named
// by the listing, and the price that listing publishes is exactly as
// well bound to the address in that case as in the other. Gating on the
// recognition source would withhold a figure this surface can correctly
// make, for a reason about how the row got in rather than about where
// the price came from.
//
// # What it is, stated rather than implied
//
// It is a listing platform's aggregate of what the TOKEN trades at, not
// an oracle's valuation of the INSTRUMENT. So:
//
//   - the reference carries [RWAReferenceListingPrice], and the
//     summary basis prose describes the mixture rather than inheriting
//     the oracle wording;
//   - NO PREMIUM is published against it, under
//     [RWAPremiumReferenceNotOracle] — a premium against an aggregate
//     of the same markets our own price samples is the market compared
//     with itself;
//   - it makes no claim that one token is one unit of anything, which
//     is the claim the oracle arm rests on and the reason that arm is
//     the stronger of the two.
//
// # One refusal, two fields — and the one place they diverge
//
// This is the only path on which the premium is refused while the
// reference valuation beside it is PUBLISHED. That is a genuine
// divergence of reasons rather than two accounts of one event: there IS
// a reference and there IS a supply, so the valuation exists; there is
// no oracle, so the comparison does not. The file header names exactly
// this shape as the case where the two fields are allowed to differ.
func rwaApplyContractReference(a *RWAAsset, entry timescale.ListingEntry, now time.Time) {
	rwaApplyListingReference(a, entry, RWAPremiumContractNotBound, now)
}

// rwaApplyListingReference is the body both arms share. `notFound` is
// the refusal to keep when the directory names nothing, because the two
// arms refuse for differently-shaped reasons: a contract row has no
// (code, issuer) to be unbound ON, and a classic row's oracle refusal
// already says something more specific than "no listing either".
//
// The classic arm reaches this only AFTER the oracle arm has refused,
// and only when that refusal was "nobody bound this pair" rather than a
// finding about a feed that IS bound. An expired feed, a non-USD feed or
// a non-positive net asset value are observations about a binding this
// index made, and a listing price does not overturn them — it answers a
// different question. Substituting one for the other would publish a
// figure while suppressing the finding that refused it.
func rwaApplyListingReference(a *RWAAsset, entry timescale.ListingEntry, notFound string, now time.Time) {
	// An address no listing named. The original refusal, unchanged, and
	// still the right one: no source binds this address to a price.
	if entry.PriceUSD == "" {
		rwaRefuseReference(a, notFound)
		return
	}
	price := ratFromOptionalString(&entry.PriceUSD)
	if price == nil || price.Sign() <= 0 {
		// Not a valuation. Refused rather than multiplied, the same way
		// the oracle path refuses a non-positive net asset value.
		rwaRefuseReference(a, RWAPremiumReferenceNotPositive)
		return
	}
	// The storage reader enforces the price bound in SQL, so a row that
	// arrives here is already inside it. Re-checked on the OBSERVATION
	// anyway, for the reason the oracle path re-checks its own: the
	// bound this surface documents has to hold however the row reached
	// it, not only on the path that was expected to deliver it.
	if entry.PricedAt.IsZero() || now.Sub(entry.PricedAt) > rwaReferenceMaxAge {
		rwaRefuseReference(a, RWAPremiumReferenceExpired)
		return
	}
	a.Reference = &RWAReference{
		PriceUSD: entry.PriceUSD,
		Source:   entry.Source,
		// The listing platform's own coin id — the key its price is
		// published under, so a reader can pull the same figure from
		// the same source. Not an ADR-0028 feed id, and it does not
		// pretend to be: the field's meaning follows `provenance`.
		Feed:       entry.ListingID,
		Quote:      "fiat:USD",
		AsOf:       WireTime(entry.PricedAt),
		Stale:      now.Sub(entry.PricedAt) > rwaReferenceStaleAfter,
		Provenance: RWAReferenceListingPrice,
	}
	a.ReferenceValuation = rwaReferenceValuationOf(a, rwaReference{priceUSD: price})
	// R-C, on the new arm. The reference is published and the premium
	// is not, and the status says which kind of figure refused it.
	a.Premium = RWAPremium{Status: RWAPremiumReferenceNotOracle}
}

// rwaListingReferencesOf indexes the admitted contract members' listing
// rows by address, for the per-row reference pass.
//
// Built from the MEMBERSHIP rather than from a second read of the
// listing directory: the row that priced an asset must be the same row
// that recognised it, or the surface could publish a price from a
// snapshot in which the address was not named.
func rwaListingReferencesOf(members []rwaContractMember) map[string]timescale.ListingEntry {
	out := make(map[string]timescale.ListingEntry, len(members))
	for _, m := range members {
		if m.listing.ListingID == "" {
			continue
		}
		out[m.contractID] = m.listing
	}
	return out
}

// rwaRefuseReference records ONE refusal in BOTH places it has to
// appear.
//
// Every rule this file enforces refuses the premium and the
// reference-priced valuation together — there is no reference, so
// neither figure exists — and the two fields must never end up telling
// two stories about one event. Writing them from a single argument at a
// single call site is what guarantees that: a status added later cannot
// be wired into one field and forgotten in the other, and the funnel,
// which reads only `reference_valuation.status`, reports exactly what
// `premium.status` says.
// rwaApplyConstantNAVReference prices a share class at the NAV its
// prospectus fixes (rwa.ConstantNAV). The reference is dated now rather
// than at the binding's verification date: the value is a standing rule
// of the fund, not an observation that ages, and the verification date
// travels in Source for the reader who wants it.
func rwaApplyConstantNAVReference(a *RWAAsset, b rwa.ConstantNAVBinding, now time.Time) {
	price := ratFromOptionalString(&b.NAVUSD)
	if price == nil || price.Sign() <= 0 {
		rwaRefuseReference(a, RWAPremiumReferenceNotPositive)
		return
	}
	a.Reference = &RWAReference{
		PriceUSD:   b.NAVUSD,
		Source:     b.Regime + "; issuer NAV page " + b.Source + " (read " + b.VerifiedOn + ")",
		Feed:       b.ISIN,
		Quote:      "fiat:USD",
		AsOf:       WireTime(now),
		Stale:      false,
		Provenance: RWAReferenceProspectusCNAV,
	}
	a.ReferenceValuation = rwaReferenceValuationOf(a, rwaReference{priceUSD: price})
	a.Premium = RWAPremium{Status: RWAPremiumReferenceNotOracle}
}

func rwaRefuseReference(a *RWAAsset, status string) {
	a.Premium = RWAPremium{Status: status}
	a.ReferenceValuation = RWAReferenceValuation{Status: status}
}

// rwaReferenceValuationOf values one row's float at the reference
// price, or states why it cannot.
//
// Called only on a row that already carries a published reference, so
// the refusals here are about the OTHER two inputs — the oracle's value
// and the chain's supply — and never about whether a reference was
// admissible, which [rwaApplyReference] has already decided.
func rwaReferenceValuationOf(a *RWAAsset, ref rwaReference) RWAReferenceValuation {
	// A zero or negative net asset value is bad data, not a valuation of
	// zero. The premium path refuses to divide by it; this path refuses
	// to multiply a float by it, and for the same reason: the result
	// would be a number the oracle did not claim. The premium reports
	// the same refusal below when a market price exists to compare.
	if ref.priceUSD == nil || ref.priceUSD.Sign() <= 0 {
		return RWAReferenceValuation{Status: RWAPremiumReferenceNotPositive}
	}
	if a.CirculatingSupply == nil {
		return RWAReferenceValuation{Status: RWAReferenceValuationNoSupply}
	}
	// The exponent, before the arithmetic that uses it. A contract row
	// whose scale could not be read carries the catalogue's default of
	// 7, which is a convention and not a reading — see
	// [RWAReferenceValuationDecimalsUnknown].
	if a.DecimalsUnresolved {
		return RWAReferenceValuation{Status: RWAReferenceValuationDecimalsUnknown}
	}
	value := rwaReferenceValueUSD(*a.CirculatingSupply, a.Decimals, ref.priceUSD)
	if value == "" {
		return RWAReferenceValuation{Status: RWAReferenceValuationNoSupply}
	}
	return RWAReferenceValuation{Status: RWAReferenceValuationPublished, ValueUSD: &value}
}

// rwaReferenceValueUSD = (circulating / 10^decimals) x referencePrice,
// as a 2-dp decimal string. Empty on any input that is not a valuation.
//
// EXACT rational arithmetic end to end (ADR-0003). Both inputs arrive
// exact and stay exact: the supply is an integer count of the smallest
// unit, and the reference price is already held as a [big.Rat] built
// from the oracle's own scaled integer by [ratFromScaledInt], so
// nothing on this path is ever a float. The 2-dp rounding happens once,
// at the end, which is what lets the summary total be the exact sum of
// the per-row strings a reader can add up by hand.
//
// The scale is the ASSET's own decimals, never a constant. A market cap
// computed against a hardcoded 7 was a real defect, and it is not a
// display defect on this path either: a 6-decimal token valued at 7
// publishes a tenth of the real figure and an 18-decimal one publishes
// a hundred billion times it. Contract-issued members make that live
// rather than theoretical, since a SEP-41 token declares its own scale.
// Guarded rather than assumed: a negative scale is not a scale, and
// [big.Int.Exp] answers a negative exponent with 1, which would publish
// the raw smallest-unit count as dollars.
func rwaReferenceValueUSD(circRaw string, decimals int, unit *big.Rat) string {
	if unit == nil || decimals < 0 {
		return ""
	}
	circ := ratFromOptionalString(&circRaw)
	// A negative supply is bad data — a legitimate float is never
	// negative — and is refused rather than published as a negative
	// valuation.
	if circ == nil || circ.Sign() < 0 {
		return ""
	}
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
	out := new(big.Rat).Quo(circ, scale)
	out.Mul(out, unit)
	return out.FloatString(2)
}

// rwaReferenceCounts totals how much of the set carries an independent
// valuation and how much of it is comparable, so a reader of the
// headline knows the coverage of the comparison without counting rows.
func rwaReferenceCounts(assets []RWAAsset) (referenced, compared int) {
	for _, a := range assets {
		if a.Reference != nil {
			referenced++
		}
		if a.Premium.Status == RWAPremiumPublished {
			compared++
		}
	}
	return referenced, compared
}
