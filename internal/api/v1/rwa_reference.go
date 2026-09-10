package v1

import (
	"context"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
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
}

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
	// not a feed, and ships empty for want of primary sources — so
	// there is no bound contract to answer either.
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
func rwaApplyReference(a *RWAAsset, snap rwaReferences, now time.Time) {
	// The scam-flag suppression comes first and covers the reference
	// too. /v1/assets withholds this issuer's own price; handing it a
	// real instrument's independent NAV instead would publish a larger
	// claim through the gap — a dollar figure on an impersonator, from a
	// source that never named it.
	if a.Valuation.Status == RWAValuationIssuerFlagged {
		rwaRefuseReference(a, RWAPremiumIssuerFlagged)
		return
	}
	// A read that did not answer knows nothing either way. Reporting it
	// as an absence would publish a finding the read did not earn, and on
	// the wire it would be indistinguishable from a genuine one.
	if !snap.available {
		rwaRefuseReference(a, RWAPremiumReferenceUnavailable)
		return
	}
	// A contract-issued member. Refused here rather than falling through
	// to the (code, issuer) binding below, which would answer with a
	// reason naming an identity this row does not have. See
	// [RWAPremiumContractNotBound] for why the available join is not
	// taken.
	if a.ContractID != "" {
		rwaRefuseReference(a, RWAPremiumContractNotBound)
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
		if rwa.OffChainReferenceCode(a.Code) {
			rwaRefuseReference(a, RWAPremiumNotInstrumentScoped)
			return
		}
		rwaRefuseReference(a, RWAPremiumNotBound)
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
		PriceUSD: ref.wire,
		Source:   ref.source,
		Feed:     ref.feed,
		Quote:    "fiat:USD",
		AsOf:     WireTime(ref.asOf),
		Stale:    now.Sub(ref.asOf) > rwaReferenceStaleAfter,
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
