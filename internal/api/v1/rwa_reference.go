package v1

import (
	"context"
	"math/big"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/tiingo"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// Oracle NAV reference and premium/discount for /v1/rwa/assets.
//
// The gap between an oracle's NAV for a tokenized treasury and the Stellar market price needs the
// oracle stream and the gated market price together. Five rules gate it; each blocks a number that
// would mean something other than what it says.
//
// R-0 bound to this (code, issuer): a code-only join would price an unrelated token against a real
// security ([rwa.InstrumentFeed], ADR-0040). R-A quoted in dollars: a bare `_FUNDAMENTAL` feed is a
// ratio in the reserve asset, so the stored quote must be `fiat:USD`. R-B prices one token (`rwa:XAU`,
// `rwa:SPXU` are not bindable). R-C published by an [external.ClassOracle]; aggregators share the
// hypertable and a premium against one compares the market with itself. R-D the market price is
// observed, not a `price_basis` peg or derivation. A refused row names its rule and never carries a zero.
//
// `reference_valuation` is the reference times the float. It rides R-0 through R-C but not R-D, so it
// is published under its own name and total, beside `market_cap_usd` and never inside it.
// `premium.status` and `reference_valuation.status` are assigned from the same constant, so a shared
// refusal cannot read as two events.

// rwaReferenceTTL bounds the reuse of one oracle-stream snapshot.
//
// The feeds publish every seconds to minutes, so 30s keeps the surface off a per-request hypertable
// scan. Far shorter than [rwaMembershipTTL]: membership moves daily, a price does not.
const rwaReferenceTTL = 30 * time.Second

// rwaReferenceStaleAfter is the age past which a reference is served
// with `stale: true`.
//
// 72 hours is the longest ordinary gap between two strikes of an instrument's value (a Friday figure
// read on Monday plus a holiday); shorter would flag every weekend on a treasury fund.
//
// It LABELS, it does not withhold: last Friday's NAV is still the current NAV on Monday. The absolute
// outer bound is [rwaReferenceMaxAge].
const rwaReferenceStaleAfter = 72 * time.Hour

// rwaReferenceMaxAge is the age past which an observation is not served
// at all.
//
// Seven days is the window LatestOracleStreams defines an ACTIVE stream by, so a live read never
// returns anything older. The check exists because a snapshot is carried forward across a failed read,
// and a sustained outage would otherwise let carried rows age past the bound that
// docs/methodology/rwa-definition.md claims is absolute.
const rwaReferenceMaxAge = 7 * 24 * time.Hour

// rwaFundNAVMaxAge is the oldest fund NAV the fund arm serves. A NAV is
// struck every business day and dated that day, so five calendar days
// spans a weekend plus a holiday; an older one means the feed stopped.
const rwaFundNAVMaxAge = 5 * 24 * time.Hour

// rwaFundNAVDecimalsPublished is the precision the fund administrator
// publishes a NAV at (the SEC-reported figure, rounded to the cent).
const rwaFundNAVDecimalsPublished = 2

// rwaNAVDisagreementTolerance is half a cent: the rounding a 2-dp NAV
// carries, so a gap within it is not a disagreement.
var rwaNAVDisagreementTolerance = big.NewRat(1, 200)

// rwaFundNAVTicker is the (code, issuer) → fund ticker join; a variable
// so a test can bind a pair the oracle arm also binds.
var rwaFundNAVTicker = rwa.FundNAVTicker

// rwaReferenceRefreshTimeout bounds one detached oracle-stream read,
// mirroring oracleFetchBudget: the read must outlive whichever request
// happened to notice the cache was cold or lapsed.
const rwaReferenceRefreshTimeout = 30 * time.Second

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
	// Stale marks a reference older than 72h, served but labelled. On a [RWAReferenceProspectusCNAV]
	// reference it marks a binding past [rwa.ConstantNAVReviewInterval]: the NAV does not age, the
	// reading of it does.
	Stale bool `json:"stale,omitempty"`
	// Provenance names WHAT KIND of figure this is. The five kinds are different claims, so the field
	// is mandatory on every served reference. [RWAReferenceOracleNAV] is an oracle's valuation of the
	// INSTRUMENT; [RWAReferenceFundNAV] the fund's own daily NAV per share; [RWAReferenceListingPrice]
	// a listing platform's aggregate of what the TOKEN trades at; [RWAReferenceProspectusCNAV] the
	// issuer's prescribed NAV; [RWAReferenceCuratorPrice] a curator's typed figure, weakest of all.
	// Folding them under one name would let a summary total describe itself with the wrong provenance.
	Provenance string `json:"provenance"`
	// DecimalsPublished is the precision the publisher states the value
	// at, when it is coarser than the wire scale suggests. Set on a
	// [RWAReferenceFundNAV] reference: a NAV published to the cent cannot
	// resolve a difference smaller than half a cent.
	DecimalsPublished *int `json:"decimals_published,omitempty"`
	// NAVDisagreement marks an oracle reference whose fund's own
	// published NAV differs from it by more than half a cent. The oracle
	// figure is still the one served; the flag says a second, weaker
	// source for the same share disagrees.
	NAVDisagreement bool `json:"nav_disagreement,omitempty"`
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
	// RWAReferenceListingPrice is the independent listing directory's own USD price for the token.
	//
	// It prices the TOKEN, not the instrument, so it claims no one-to-one correspondence and cannot
	// produce a premium: a premium against an aggregate of the markets is the market compared with
	// itself. It is available to any contract row the directory NAMES, whichever C2 arm admitted it,
	// because the entry is a USD price bound to a 56-character address by a party that did not read our
	// directory. A contract the listing never named has no price to serve.
	RWAReferenceListingPrice = "listing_platform_price"
	// RWAReferenceProspectusCNAV is the issuer's published NAV for a share class whose fund rules fix
	// it, bound on the exact (code, issuer) in rwa.ConstantNAV. Nobody independent measured it, but the
	// value is prescribed by a prospectus. Taken only when neither an oracle binding nor a usable
	// listing price (positive, parseable, inside the reference bound) exists for the row.
	RWAReferenceProspectusCNAV = "prospectus_constant_nav"
	// RWAReferenceFundNAV is the fund's own daily NAV per share (the SEC-reported figure, via Tiingo),
	// bound on the exact (code, issuer) in rwa.FundNAVTicker. It ranks below the oracle arm (the
	// issuer's statement relayed by a vendor) and above the listing arm (it values the share, not the
	// token's own markets). Published to the cent, stated in `decimals_published`.
	RWAReferenceFundNAV = "fund_nav"
)

// RWAReferenceValuation is the token's circulating supply valued at the REFERENCE
// price (the oracle's value of the instrument), not at anything the market was
// observed paying.
//
// It is the opposite of `valuation.market_cap_usd` and the two are never folded together: that one
// is gated (thin-market substance, dust liquidity, scam-issuer suppression) and backed by a settled
// trade. This figure is a CLAIM: nothing was paid and no liquidity gate can measure it, so it may not
// be reported as a market capitalisation. The distinction is carried by the field name, by the
// sibling `reference` block naming feed and vintage, and by `summary.reference_valuation.basis`.
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
	// RWAReferenceValuationDecimalsUnknown — a reference and a supply exist but the token's declared
	// SCALE does not, so there is no exponent to divide the float by. Same refusal as
	// [RWAValuationDecimalsUnknown], with more at stake: this basis may be the only number on the
	// row, and defaulting the exponent would publish a total wrong by a power of ten as published.
	RWAReferenceValuationDecimalsUnknown = "decimals_unavailable"
	// RWAReferenceValuationNoSupply — a reference exists but no usable circulating-supply reading
	// does (non-numeric or negative counts as none). The premium is unaffected, since it compares
	// two prices and needs no supply, which is why this status is the valuation's own.
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
	// RWAPremiumContractNotBound: the member is CONTRACT-issued and nothing binds a contract address
	// to an oracle feed. Separate from [RWAPremiumNotBound], which names a (code, issuer) pair.
	//
	// The symbol join is REFUSED: a contract admitted on [rwa.BasisContractOracleFeed] got in via
	// its on-chain SEP-41 symbol, but the contract authors that symbol itself, so pricing by it is
	// the code-keyed join this file exists to refuse. [rwa.ContractInstrumentBindings] records
	// instrument and class, not a feed, so it cannot answer either.
	RWAPremiumContractNotBound = "reference_contract_not_bound"
	// RWAPremiumNotBound — no curated binding ties this exact (code, issuer) to an oracle feed.
	// Usually a code collision (a token wearing an instrument's ticker); it also covers a genuine
	// issuer nobody has bound yet, which is why the refusal is reported rather than absorbed.
	RWAPremiumNotBound = "reference_not_bound"
	// RWAPremiumReferenceISINMismatch — the ISIN the issuer's SEP-1 declares contradicts the
	// constant-NAV table (bound to a different class, or the ISIN belongs to a class bound on
	// another pair). Valuing it would attach one security's NAV to a row naming another, and a
	// changed declaration suggests a reorganised class, so nothing is served until re-verified.
	RWAPremiumReferenceISINMismatch = "reference_isin_mismatch"
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
	// RWAPremiumReferenceNotOracle — the reference is a LISTING PRICE, not an oracle's valuation of
	// the instrument, so no premium is computed (R-C). The reference valuation beside it can still
	// be PUBLISHED. A listing price aggregates the same markets our own price comes from, so the gap
	// would measure two samples of one market, not a premium. The arithmetic is available and
	// refused anyway: a number that can be computed does not thereby mean something.
	// [RWAPremiumReferenceNotOracleCNAV] and [RWAPremiumReferenceNotOracleCurator] carry the same
	// refusal for the other provenances so the wire value names the figure actually refused.
	RWAPremiumReferenceNotOracle = "reference_is_a_listing_price"
	// RWAPremiumReferenceNotOracleCNAV — the reference is a prospectus constant NAV, not an oracle
	// valuation, so no premium is computed. See [RWAReferenceProspectusCNAV].
	RWAPremiumReferenceNotOracleCNAV = "reference_is_a_prospectus_nav"
	// RWAPremiumReferenceNotOracleCurator — the reference is a curator's uploaded price, not an
	// oracle valuation, so no premium is computed. See [RWAReferenceCuratorPrice] in rwa_curated.go.
	RWAPremiumReferenceNotOracleCurator = "reference_is_a_curator_price"
	// RWAPremiumReferenceNotOracleFundNAV — the reference is a fund's published NAV, and no premium
	// is computed: a NAV rounded to the cent carries half a cent of error, which on a one-dollar
	// share is a half-percent premium the market never paid.
	RWAPremiumReferenceNotOracleFundNAV = "reference_is_a_fund_nav"
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
	// fundNAV holds the latest dollar NAV per fund ticker, from the one
	// source the fund bindings name. Keyed by the ticker verbatim.
	fundNAV map[string]rwaReference
	// globalUSD holds the newest aggregator USD price per global ticker
	// (crypto:USDC …), keyed by the canonical asset id. Read by the asset
	// surfaces' global-market fill ([Server.applyGlobalMarket]), not here.
	globalUSD map[string]rwaReference
	// available is false when no oracle reader is wired or the read
	// failed. Distinguished from "the oracles publish nothing for these
	// instruments", which is a finding a failed read may not make.
	available bool
}

// addSideFeed files u under fundNAV or globalUSD and reports whether
// either claimed it, so the instrument-feed rules below never see it.
func (r *rwaReferences) addSideFeed(u canonical.OracleUpdate) bool {
	return rwaAddFundNAV(r.fundNAV, u) || addGlobalMarketRow(r.globalUSD, u)
}

// rwaReferenceSnapshot reduces one oracle-stream read to the references
// this surface may publish.
//
// It runs over the SAME rows /v1/oracle/streams serves (one per (source, asset, quote) in the
// trailing 7d) rather than a per-code query: one read serves the whole set at a single scan, and a
// feed absent from that window is not an active stream, so the row carries no reference instead of a
// figure from a quiet source.
func rwaReferenceSnapshotFrom(updates []canonical.OracleUpdate) rwaReferences {
	out := rwaReferences{
		byFeed:    map[string]rwaReference{},
		nonUSD:    map[string]string{},
		fundNAV:   map[string]rwaReference{},
		globalUSD: map[string]rwaReference{},
		available: true,
	}
	for _, u := range updates {
		if out.addSideFeed(u) {
			continue
		}
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

// rwaAddFundNAV keeps the newest dollar NAV for one fund ticker, and
// reports whether u was a fund-NAV row at all.
func rwaAddFundNAV(into map[string]rwaReference, u canonical.OracleUpdate) bool {
	if u.Source != tiingo.SourceName || u.Asset.Type != canonical.AssetOracleRaw {
		return false
	}
	if !isUSDQuote(u.Quote) {
		return true
	}
	if prev, ok := into[u.Asset.Code]; ok && !u.Timestamp.After(prev.asOf) {
		return true
	}
	into[u.Asset.Code] = rwaReference{
		priceUSD: ratFromScaledInt(u.Price.BigInt(), u.Decimals),
		wire:     trimScaledZeros(scaledDecimalString(u.Price.BigInt(), u.Decimals), rwaFundNAVDecimalsPublished),
		source:   u.Source,
		feed:     u.Asset.String(),
		asOf:     u.Timestamp,
	}
	return true
}

// trimScaledZeros drops trailing fractional zeros down to minFrac places,
// so a cent-precision NAV stored at a finer scale reads as published
// without discarding any non-zero digit.
func trimScaledZeros(s string, minFrac int) string {
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return s
	}
	end := len(s)
	for end > dot+1+minFrac && s[end-1] == '0' {
		end--
	}
	return s[:end]
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
// On a failed read it serves the last good snapshot, else an UNAVAILABLE one. Unavailable is not
// empty: an empty snapshot would tell every row "no oracle publishes your instrument", a finding a
// failed read may not make.
func (s *Server) cachedRWAReferences(ctx context.Context) rwaReferences {
	if s.Oracle == nil {
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
	//nolint:gosec,contextcheck // G118 / contextcheck:
	// intentional detached fill — the waking caller's own ctx must not
	// abort a fill every other single-flighted caller depends on; see
	// fillRWAReferences's doc comment.
	go s.fillRWAReferences(done)

	select {
	case <-done:
	case <-ctx.Done():
		return rwaReferences{}
	}
	s.rwaRefMu.Lock()
	var snap rwaReferences
	if s.rwaRefCache != nil {
		snap = *s.rwaRefCache
	}
	s.rwaRefMu.Unlock()
	return snap
}

// fillRWAReferences runs the oracle-stream read and releases the single-flight gate even on panic.
// A panic out of LatestOracleStreams that skipped the release would leave rwaRefFlight pointing at
// a channel nobody closes, wedging /v1/rwa/assets for the life of the process. It runs on its own
// budget, like oracleFetchBudget, because one caller's disconnect must not abort a shared fill.
func (s *Server) fillRWAReferences(done chan struct{}) {
	defer close(done)
	defer func() {
		if rec := recover(); rec != nil {
			worker.Report(s.logger, "api-rwa-references-refresh", rec)
		}
		s.rwaRefMu.Lock()
		s.rwaRefFlight = nil
		s.rwaRefMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), rwaReferenceRefreshTimeout)
	defer cancel()

	var built rwaReferences
	updates, err := s.Oracle.LatestOracleStreams(ctx)
	if err != nil {
		s.logger.Warn("rwa references: oracle stream read failed", "err", err)
	} else {
		built = rwaReferenceSnapshotFrom(updates)
	}

	s.rwaRefMu.Lock()
	if built.available {
		s.rwaRefCache = &built
		s.rwaRefAt = time.Now()
	}
	s.rwaRefMu.Unlock()
}

// ─── per-row application ────────────────────────────────────────────

// rwaApplyReference attaches the oracle reference and the premium to one
// served row, or the reason neither is there.
//
// The order of the refusals is the order of the rules and is load-bearing for the reason reported:
// a flagged issuer is answered as flagged first, since that is about the issuer, not the data.
func rwaApplyReference(
	a *RWAAsset,
	snap rwaReferences,
	listings map[string]timescale.ListingEntry,
	classicListings map[string]timescale.ListingEntry,
	now time.Time,
) {
	// The scam-flag suppression comes first and covers the reference too: /v1/assets withholds this
	// issuer's price, and handing it a real instrument's NAV instead would publish a larger claim.
	if a.Valuation.Status == RWAValuationIssuerFlagged {
		rwaRefuseReference(a, RWAPremiumIssuerFlagged)
		return
	}
	// A contract-issued member is answered BEFORE the oracle snapshot is consulted: it has no
	// (code, issuer) identity to bind, and its reference never comes from the oracle stream, so an
	// oracle outage must not withhold its figure.
	if a.ContractID != "" {
		rwaApplyContractReference(a, listings[a.ContractID], now)
		return
	}
	if rwa.ConstantNAVISINConflict(a.Code, a.Issuer, a.AnchorAsset) {
		rwaRefuseReference(a, RWAPremiumReferenceISINMismatch)
		return
	}

	// R-0 — the binding, on the exact (code, issuer). This is the join,
	// and it is the whole reason a token that merely shares a ticker with
	// a real instrument cannot be handed that instrument's valuation.
	feed, bound := rwa.InstrumentFeed(a.Code, a.Issuer)
	if !bound {
		// Both refusals withhold, but the specific one is worth reporting: a token coded XAU is not
		// unbound by oversight, the oracle of that name prices a troy ounce of metal. A fund share's
		// own NAV outranks the listing and prospectus arms; with none in the snapshot the row falls
		// through to them.
		if rwaApplyFundNAVReference(a, snap, now) {
			return
		}
		notFound := RWAPremiumNotBound
		if rwa.OffChainReferenceCode(a.Code) {
			notFound = RWAPremiumNotInstrumentScoped
		}
		// The listing row for this exact asset, resolved by the SAME rule /v1/assets uses
		// ([listingEntryIn]): the `CODE-GISSUER` id first, then the derived Stellar Asset Contract
		// address. The directory lists each asset under ONE of those forms with no pattern, so a
		// classic-only key would refuse a SAC-listed member as unbound.
		//
		// Never the code: many assets share a code (BENJI), and a code-keyed lookup is the
		// attacker-authored-pricing class, the same reason [rwa.InstrumentFeed] is keyed on the pair.
		// Both address forms derive from the exact (code, issuer), so neither widens the key.
		listing, _, _ := listingEntryIn(classicListings, a.AssetID)
		// A CNAV share class bound in rwa.ConstantNAV is valued at its prospectus NAV. A listing
		// price wins only when it is a VALUATION the listing arm would serve; a zero, garbage or
		// expired third-party row must not erase the prospectus figure.
		if _, refusal := rwaListingPrice(listing, notFound, now); refusal != "" {
			if cnav, ok := rwa.ConstantNAV(a.Code, a.Issuer); ok {
				rwaApplyConstantNAVReference(a, cnav, now)
				return
			}
		}
		// No oracle binds this pair, which is a statement about one source only. The listing
		// directory is the same address-exact evidence that already prices the contract arm, so
		// classic rows read it too rather than being reported unpriced.
		rwaApplyListingReference(a, listing, notFound, now)
		return
	}

	// A read that did not answer knows nothing either way; reporting an absence would publish a
	// finding it did not earn. Only an oracle-bound row is refused on it: the unbound arm above
	// falls through to the listing arm.
	if !snap.available {
		rwaRefuseReference(a, RWAPremiumReferenceUnavailable)
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

	// A snapshot carried forward across a sustained read failure can age past the 7-day window, so
	// the bound is enforced on the OBSERVATION, not the snapshot.
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
	a.Reference.NAVDisagreement = rwaFundNAVDisagrees(a, snap, ref.priceUSD, now)
	// Only with the reference attached: a supply-valued figure whose feed and vintage were not
	// published would be a dollar total with no traceable source.
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

// rwaApplyContractReference attaches the LISTING-priced reference to a contract member, or the
// reason there is none.
//
// Nothing binds a contract ADDRESS to an oracle feed (symbol joins are code-keyed, which R-0
// refuses). A listing directory row instead NAMES the exact address and publishes a USD price for
// it, so no code is matched. It applies to any contract row the listing names, not only ones it
// ADMITTED: gating on how the row got in would withhold a correct figure.
//
// The figure is a listing aggregate of what the TOKEN trades at, so the reference carries
// [RWAReferenceListingPrice] and NO PREMIUM is published ([RWAPremiumReferenceNotOracle]): it would
// compare the market with itself. This is the case where the premium is refused while the reference
// valuation beside it is PUBLISHED.
func rwaApplyContractReference(a *RWAAsset, entry timescale.ListingEntry, now time.Time) {
	rwaApplyListingReference(a, entry, RWAPremiumContractNotBound, now)
}

// rwaApplyListingReference is the body both arms share. `notFound` is the refusal to keep when the
// directory names nothing: a contract row has no (code, issuer) to be unbound ON, and a classic
// row's oracle refusal is more specific than "no listing either".
//
// The classic arm reaches this only when the oracle refusal was "nobody bound this pair". An
// expired, non-USD or non-positive feed is a finding about a binding this index made, and a listing
// price does not overturn it; substituting one would publish a figure while suppressing the finding.
func rwaApplyListingReference(a *RWAAsset, entry timescale.ListingEntry, notFound string, now time.Time) {
	price, refusal := rwaListingPrice(entry, notFound, now)
	if refusal != "" {
		rwaRefuseReference(a, refusal)
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

// rwaListingPrice resolves a listing row to the positive price the
// listing arm serves, or to the refusal it reports instead. The CNAV
// precedence gate and the arm share it so they cannot disagree about
// what counts as a listing valuation.
func rwaListingPrice(entry timescale.ListingEntry, notFound string, now time.Time) (*big.Rat, string) {
	// An address no listing named. The original refusal, unchanged, and
	// still the right one: no source binds this address to a price.
	if entry.PriceUSD == "" {
		return nil, notFound
	}
	price := ratFromOptionalString(&entry.PriceUSD)
	if price == nil || price.Sign() <= 0 {
		// Not a valuation. Refused rather than multiplied, the same way
		// the oracle path refuses a non-positive net asset value.
		return nil, RWAPremiumReferenceNotPositive
	}
	// The storage reader enforces the price bound in SQL; the age is re-checked here so the
	// documented bound holds however the row arrived.
	if entry.PricedAt.IsZero() || now.Sub(entry.PricedAt) > rwaReferenceMaxAge {
		return nil, RWAPremiumReferenceExpired
	}
	return price, ""
}

// rwaListingReferencesOf indexes the admitted contract members' listing
// rows by address, for the per-row reference pass.
//
// Built from the MEMBERSHIP, not a second listing read: the row that priced an asset must be the
// row that recognised it, or a price could come from a snapshot that did not name the address.
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

// rwaRefuseReference records ONE refusal in BOTH places it must appear (premium and
// reference valuation), so the two cannot tell different stories; the funnel reads only
// `reference_valuation.status`.
//
// rwaApplyConstantNAVReference prices a share class at the NAV its prospectus fixes
// (rwa.ConstantNAV). The reference is dated now because the value is a standing rule of the fund,
// not an observation that ages; the verification date travels in Source. Past the binding's
// ReviewBy the row is served `stale: true`, labelled not withheld, like the 72-hour oracle bound.
func rwaApplyConstantNAVReference(a *RWAAsset, b rwa.ConstantNAVBinding, now time.Time) {
	price := ratFromOptionalString(&b.NAVUSD)
	if price == nil || price.Sign() <= 0 {
		rwaRefuseReference(a, RWAPremiumReferenceNotPositive)
		return
	}
	due := b.ReviewDue(now)
	source := b.Regime + "; issuer NAV page " + b.Source + " (read " + b.VerifiedOn + "; review by " + b.ReviewBy + ")"
	if due {
		source = b.Regime + "; issuer NAV page " + b.Source + " (read " + b.VerifiedOn + "; re-verification was due " + b.ReviewBy + " and has not been recorded — the binding is due for re-verification)"
	}
	a.Reference = &RWAReference{
		PriceUSD:   b.NAVUSD,
		Source:     source,
		Feed:       b.ISIN,
		Quote:      "fiat:USD",
		AsOf:       WireTime(now),
		Stale:      due,
		Provenance: RWAReferenceProspectusCNAV,
	}
	a.ReferenceValuation = rwaReferenceValuationOf(a, rwaReference{priceUSD: price})
	a.Premium = RWAPremium{Status: RWAPremiumReferenceNotOracleCNAV}
}

// rwaFundNAVDisagrees reports whether the fund NAV bound to this row's
// (code, issuer), when the snapshot holds one no older than
// [rwaFundNAVMaxAge], disagrees with the oracle value.
func rwaFundNAVDisagrees(a *RWAAsset, snap rwaReferences, oracle *big.Rat, now time.Time) bool {
	ticker, ok := rwaFundNAVTicker(a.Code, a.Issuer)
	if !ok {
		return false
	}
	nav, ok := snap.fundNAV[ticker]
	if !ok || now.Sub(nav.asOf) > rwaFundNAVMaxAge {
		return false
	}
	return rwaNAVDisagrees(oracle, nav.priceUSD)
}

// rwaNAVDisagrees reports whether two per-share values differ by more
// than [rwaNAVDisagreementTolerance]; exactly half a cent agrees.
func rwaNAVDisagrees(value, nav *big.Rat) bool {
	if value == nil || nav == nil {
		return false
	}
	diff := new(big.Rat).Sub(value, nav)
	return diff.Abs(diff).Cmp(rwaNAVDisagreementTolerance) > 0
}

// rwaApplyFundNAVReference prices a fund-share row at its fund's published
// NAV, reporting whether it decided the row. A bound row with no NAV in
// the snapshot is left undecided so the lower arms can answer it; a NAV
// past [rwaFundNAVMaxAge] is refused, as an expired oracle feed is.
func rwaApplyFundNAVReference(a *RWAAsset, snap rwaReferences, now time.Time) bool {
	ticker, ok := rwaFundNAVTicker(a.Code, a.Issuer)
	if !ok {
		return false
	}
	nav, ok := snap.fundNAV[ticker]
	if !ok {
		return false
	}
	if now.Sub(nav.asOf) > rwaFundNAVMaxAge {
		rwaRefuseReference(a, RWAPremiumReferenceExpired)
		return true
	}
	published := rwaFundNAVDecimalsPublished
	a.Reference = &RWAReference{
		PriceUSD:          nav.wire,
		Source:            nav.source,
		Feed:              nav.feed,
		Quote:             "fiat:USD",
		AsOf:              WireTime(nav.asOf),
		Stale:             now.Sub(nav.asOf) > rwaReferenceStaleAfter,
		Provenance:        RWAReferenceFundNAV,
		DecimalsPublished: &published,
	}
	a.ReferenceValuation = rwaReferenceValuationOf(a, nav)
	a.Premium = RWAPremium{Status: RWAPremiumReferenceNotOracleFundNAV}
	return true
}

func rwaRefuseReference(a *RWAAsset, status string) {
	a.Premium = RWAPremium{Status: status}
	a.ReferenceValuation = RWAReferenceValuation{Status: status}
}

// rwaReferenceValuationOf values one row's float at the reference
// price, or states why it cannot.
//
// Called only on a row that already carries a published reference, so the refusals here concern the
// price value and the supply, never whether a reference was admissible ([rwaApplyReference]).
func rwaReferenceValuationOf(a *RWAAsset, ref rwaReference) RWAReferenceValuation {
	// A zero or negative net asset value is bad data, not a valuation of zero; the premium path
	// refuses to divide by it and this path refuses to multiply by it.
	if ref.priceUSD == nil || ref.priceUSD.Sign() <= 0 {
		return RWAReferenceValuation{Status: RWAPremiumReferenceNotPositive}
	}
	if a.CirculatingSupply == nil {
		return RWAReferenceValuation{Status: RWAReferenceValuationNoSupply}
	}
	// The exponent, before the arithmetic that uses it. A contract row
	// whose scale could not be read carries none — see
	// [RWAReferenceValuationDecimalsUnknown].
	if a.Decimals == nil {
		return RWAReferenceValuation{Status: RWAReferenceValuationDecimalsUnknown}
	}
	value := rwaReferenceValueUSD(*a.CirculatingSupply, *a.Decimals, ref.priceUSD)
	if value == "" {
		return RWAReferenceValuation{Status: RWAReferenceValuationNoSupply}
	}
	return RWAReferenceValuation{Status: RWAReferenceValuationPublished, ValueUSD: &value}
}

// rwaReferenceValueUSD = (circulating / 10^decimals) x referencePrice,
// as a 2-dp decimal string. Empty on any input that is not a valuation.
//
// Exact rational arithmetic end to end (ADR-0003), never a float. The 2-dp rounding happens once,
// at the end, so the summary total is the exact sum of the per-row strings.
//
// The scale is the ASSET's own decimals, never a constant: a SEP-41 token declares its own, and a
// wrong one is off by powers of ten. A negative scale is refused because [big.Int.Exp] answers a
// negative exponent with 1, which would publish the raw smallest-unit count as dollars.
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
