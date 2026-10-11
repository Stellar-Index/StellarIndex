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

// rwaReferenceTTL bounds reuse of one oracle-stream snapshot. 30s keeps the surface off a
// per-request hypertable scan; far shorter than [rwaMembershipTTL] because prices move faster.
const rwaReferenceTTL = 30 * time.Second

// rwaReferenceStaleAfter is the age past which a reference is served with `stale: true`. 72h
// covers a weekend plus a holiday; shorter would flag every weekend on a treasury fund. It labels
// and does not withhold: Friday's NAV is still the current NAV on Monday. See [rwaReferenceMaxAge].
const rwaReferenceStaleAfter = 72 * time.Hour

// rwaReferenceMaxAge is the age past which an observation is not served at all. Seven days is the
// window LatestOracleStreams defines an ACTIVE stream by; the check exists because a snapshot is
// carried forward across a failed read, so carried rows must not age past the documented bound.
const rwaReferenceMaxAge = 7 * 24 * time.Hour

// rwaFundNAVMaxAge is the oldest fund NAV served. Five days spans a weekend plus a holiday;
// older means the feed stopped.
const rwaFundNAVMaxAge = 5 * 24 * time.Hour

// rwaFundNAVDecimalsPublished is the precision a fund NAV is published at (to the cent).
const rwaFundNAVDecimalsPublished = 2

// rwaNAVDisagreementTolerance is half a cent, the rounding a 2-dp NAV carries.
var rwaNAVDisagreementTolerance = big.NewRat(1, 200)

// rwaFundNAVTicker is the (code, issuer) to fund ticker join; a variable so a test can override it.
var rwaFundNAVTicker = rwa.FundNAVTicker

// rwaReferenceRefreshTimeout bounds one detached oracle-stream read (mirrors oracleFetchBudget);
// it must outlive the request that happened to notice the cache was cold.
const rwaReferenceRefreshTimeout = 30 * time.Second

// ─── wire shape ─────────────────────────────────────────────────────

// RWAReference is an independent oracle's valuation of the real-world instrument an admitted
// token declares it anchors to. It is NOT this platform's price for the token and is not derived
// from any Stellar market; both figures are served so readers see them independently, not just
// their ratio.
type RWAReference struct {
	// PriceUSD is the oracle's value verbatim at the feed's own decimal scale (a decimal string).
	// Not re-rounded: the scale states the oracle's precision.
	PriceUSD string `json:"price_usd"`
	// Source is the registered oracle that published it.
	Source string `json:"source"`
	// Feed is the canonical asset id the oracle priced (`rwa:<CODE>`), so a reader can check the
	// figure against /v1/oracle/latest.
	Feed string `json:"feed"`
	// Quote is the denominator, always `fiat:USD` on a served row. On the wire because a NAV feed
	// denominated in a reserve asset is a ratio, invisible in the number alone.
	Quote string `json:"quote"`
	// AsOf is when the oracle published it; the market price beside it carries its own as_of.
	AsOf WireTime `json:"as_of"`
	// Stale marks a reference older than 72h, served but labelled. On a [RWAReferenceProspectusCNAV]
	// reference it marks a binding past [rwa.ConstantNAVReviewInterval]: the NAV does not age, the
	// reading of it does.
	Stale bool `json:"stale,omitempty"`
	// Provenance names WHAT KIND of figure this is (oracle instrument NAV, fund NAV, listing price,
	// prospectus constant NAV, curator price). They are different claims, so the field is mandatory;
	// folding them would let a summary total describe itself with the wrong provenance.
	Provenance string `json:"provenance"`
	// DecimalsPublished is the publisher's stated precision when coarser than the wire scale
	// suggests (a fund NAV to the cent cannot resolve less than half a cent).
	DecimalsPublished *int `json:"decimals_published,omitempty"`
	// NAVDisagreement marks an oracle reference whose fund's own NAV differs by more than half a
	// cent. The oracle figure is still served; the flag says a weaker source disagrees.
	NAVDisagreement bool `json:"nav_disagreement,omitempty"`
}

// RWAReference provenances.
const (
	// RWAReferenceOracleNAV is a registered oracle feed's value for the instrument, joined through
	// the curated (code, issuer) binding. The strongest arm.
	RWAReferenceOracleNAV = "oracle_instrument_nav"
	// RWAReferenceListingPrice is the listing directory's USD price for the token. It prices the
	// TOKEN, not the instrument, so it cannot produce a premium (that compares the market with
	// itself). Available to any contract row the directory names, whichever arm admitted it.
	RWAReferenceListingPrice = "listing_platform_price"
	// RWAReferenceProspectusCNAV is the issuer's NAV for a share class whose fund rules fix it,
	// bound on the exact (code, issuer) in rwa.ConstantNAV. Taken only when neither an oracle
	// binding nor a usable listing price (positive, parseable, inside the reference bound) exists.
	RWAReferenceProspectusCNAV = "prospectus_constant_nav"
	// RWAReferenceFundNAV is the fund's daily NAV per share, bound on the exact (code, issuer) in
	// rwa.FundNAVTicker. Ranks below the oracle arm and above the listing arm (it values the share,
	// not the token's markets). Published to the cent, stated in `decimals_published`.
	RWAReferenceFundNAV = "fund_nav"
)

// RWAReferenceValuation is the token's circulating supply valued at the REFERENCE price, not at
// anything the market was observed paying.
//
// It is never folded into `valuation.market_cap_usd`: that one is liquidity-gated and backed by a
// settled trade; this figure is a claim no gate can measure and may not be called a market cap.
type RWAReferenceValuation struct {
	// Status is the single authority on why there is no figure. Where a rule refuses the reference
	// itself it carries the SAME string as `premium.status`, assigned together.
	Status string `json:"status"`
	// ValueUSD is circulating supply (scaled by the asset's decimals) times the reference price, as
	// a 2-dp decimal string from exact rational arithmetic, never a float. Present iff published.
	ValueUSD *string `json:"value_usd,omitempty"`
}

// RWAReferenceValuation statuses NOT shared with the premium; every other value is an RWAPremium
// constant, because the rule that refused the premium refused this too.
const (
	// RWAReferenceValuationPublished — a reference price and a supply reading were both available.
	RWAReferenceValuationPublished = "published"
	// RWAReferenceValuationDecimalsUnknown — the token's declared scale is missing, so there is no
	// exponent to divide by. Defaulting it would publish a total wrong by a power of ten, and this
	// basis may be the only number on the row.
	RWAReferenceValuationDecimalsUnknown = "decimals_unavailable"
	// RWAReferenceValuationNoSupply — no usable circulating-supply reading (non-numeric or negative
	// counts as none). The premium is unaffected: it compares two prices and needs no supply.
	RWAReferenceValuationNoSupply = "supply_unavailable"
)

// RWAPremium is the token's market price measured against the oracle's valuation of the
// instrument, or the reason there is no such figure.
type RWAPremium struct {
	Status string `json:"status"`
	// Pct is (market − reference) / reference × 100 as a decimal string. Present only when Status
	// is published, never zero-filled: no comparison is not the same statement as "trades at par".
	Pct *string `json:"pct,omitempty"`
}

// RWAPremium status values.
const (
	// RWAPremiumPublished — both figures exist and are comparable.
	RWAPremiumPublished = "published"
	// RWAPremiumIssuerFlagged — the issuer carries a scam-class directory tag. No valuation is
	// published for it, including a third party's: an impersonator handed a real instrument's NAV
	// is exactly the claim the flag exists to deny.
	RWAPremiumIssuerFlagged = "withheld_issuer_flagged"
	// RWAPremiumContractNotBound: the member is CONTRACT-issued and nothing binds a contract address
	// to an oracle feed. The symbol join is REFUSED: the contract authors its own SEP-41 symbol, so
	// pricing by it is the code-keyed join this file exists to refuse.
	RWAPremiumContractNotBound = "reference_contract_not_bound"
	// RWAPremiumNotBound — no curated binding ties this exact (code, issuer) to an oracle feed.
	// Usually a code collision (a token wearing an instrument's ticker), or an issuer nobody has
	// bound yet; reported rather than absorbed.
	RWAPremiumNotBound = "reference_not_bound"
	// RWAPremiumReferenceISINMismatch — the ISIN the issuer's SEP-1 declares contradicts the
	// constant-NAV table. Valuing it would attach one security's NAV to a row naming another, and a
	// changed declaration suggests a reorganised class, so nothing is served until re-verified.
	RWAPremiumReferenceISINMismatch = "reference_isin_mismatch"
	// RWAPremiumNoReference — the pair IS bound, but the oracle stream carries no row for its feed.
	RWAPremiumNoReference = "no_reference_feed"
	// RWAPremiumReferenceUnavailable — the oracle read failed, so nothing is known either way. A
	// failed read must not report an absence as a finding.
	RWAPremiumReferenceUnavailable = "reference_unavailable"
	// RWAPremiumReferenceExpired — the bound feed's latest observation is older than the 7-day
	// active-stream window. Reached only when a snapshot is carried across a sustained read failure.
	RWAPremiumReferenceExpired = "reference_expired"
	// RWAPremiumNotInstrumentScoped — the feed prices an off-chain quantity in its own unit (a troy
	// ounce, one fund share) rather than one token; a ratio to a token price is a unit conversion.
	RWAPremiumNotInstrumentScoped = "reference_not_instrument_scoped"
	// RWAPremiumReferenceNotUSD — the stored quote is not fiat:USD, so the value is a ratio in a
	// reserve asset, not a dollar figure. Not converted here.
	RWAPremiumReferenceNotUSD = "reference_not_usd_denominated"
	// RWAPremiumNoMarketPrice — a reference exists but no served USD market price does. The
	// reference itself is still published.
	RWAPremiumNoMarketPrice = "no_market_price"
	// RWAPremiumMarketNotObserved — the served price is a declared peg or a derivation, not a market
	// observation; a premium against it would report the issuer's claim as a market finding.
	RWAPremiumMarketNotObserved = "market_price_not_observed"
	// RWAPremiumReferenceNotPositive — the oracle published a non-positive value; nothing divides by it.
	RWAPremiumReferenceNotPositive = "reference_not_positive"
	// RWAPremiumReferenceNotOracle: the reference is a LISTING PRICE, so no premium is computed (R-C)
	// though the reference valuation can still be PUBLISHED. A listing price aggregates the same
	// markets our price comes from, so the gap would compare two samples of one market.
	RWAPremiumReferenceNotOracle = "reference_is_a_listing_price"
	// RWAPremiumReferenceNotOracleCNAV — the reference is a prospectus constant NAV, not an oracle
	// valuation, so no premium is computed. See [RWAReferenceProspectusCNAV].
	RWAPremiumReferenceNotOracleCNAV = "reference_is_a_prospectus_nav"
	// RWAPremiumReferenceNotOracleCurator — the reference is a curator's uploaded price; no premium.
	// See [RWAReferenceCuratorPrice] in rwa_curated.go.
	RWAPremiumReferenceNotOracleCurator = "reference_is_a_curator_price"
	// RWAPremiumReferenceNotOracleFundNAV — a fund NAV rounded to the cent carries half a cent of
	// error, which on a one-dollar share is a half-percent premium the market never paid.
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

// rwaReferences is one oracle-stream snapshot reduced to what this surface may publish, keyed by
// the FEED code exactly as the allow-list spells it, never folded. The asset-to-feed join happens
// through [rwa.InstrumentFeed]; this map is only the feed-side half.
type rwaReferences struct {
	// byFeed holds the references that passed R-A and R-C.
	byFeed map[string]rwaReference
	// nonUSD records feeds whose only oracle rows are not dollar-denominated (value: the quote's
	// canonical id), so a row can state THAT rather than the weaker "no feed".
	nonUSD map[string]string
	// fundNAV holds the latest dollar NAV per fund ticker, keyed by the ticker verbatim.
	fundNAV map[string]rwaReference
	// globalUSD holds the newest aggregator USD price per global ticker, keyed by canonical asset
	// id. Read by [Server.applyGlobalMarket], not here.
	globalUSD map[string]rwaReference
	// available is false when no oracle reader is wired or the read failed; a failed read may not
	// claim "the oracles publish nothing for these instruments".
	available bool
}

// addSideFeed files u under fundNAV or globalUSD and reports whether
// either claimed it, so the instrument-feed rules below never see it.
func (r *rwaReferences) addSideFeed(u canonical.OracleUpdate) bool {
	return rwaAddFundNAV(r.fundNAV, u) || addGlobalMarketRow(r.globalUSD, u)
}

// rwaReferenceSnapshot reduces one oracle-stream read to the references this surface may publish.
// It runs over the SAME rows /v1/oracle/streams serves (trailing 7d) rather than a per-code query:
// one scan serves the whole set, and a feed absent from that window is not an active stream.
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
		// Only instrument feeds; whether a Stellar asset may be answered with one is R-0's
		// question, asked per row against the curated binding.
		if u.Asset.Type != canonical.AssetRWA {
			continue
		}
		// R-C: an oracle, not an aggregator writing into the same table for divergence.
		if external.Lookup(u.Source).Class != external.ClassOracle {
			continue
		}
		// The stored feed code verbatim: ParseAsset only admits the exact spelling, so no folding.
		key := u.Asset.Code
		// R-A: dollars, read off the stored row.
		if !isUSDQuote(u.Quote) {
			// Keep only the first so the reason is stable across several non-USD legs.
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
		// Several oracles may price one instrument: take the most recent, tie-broken on source
		// name so the served figure does not depend on scan order.
		if prev, ok := out.byFeed[key]; ok {
			if prev.asOf.After(ref.asOf) ||
				(prev.asOf.Equal(ref.asOf) && prev.source <= ref.source) {
				continue
			}
		}
		out.byFeed[key] = ref
	}
	// A feed whose USD leg was admitted is not missing a reference; the reason must describe the
	// row that would be served.
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

// trimScaledZeros drops trailing fractional zeros down to minFrac places, never a non-zero digit.
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

// isUSDQuote reports whether an oracle row's denominator is the dollar. Exact on the fiat asset,
// never a code comparison: `crypto:USDC` and `fiat:USD` are different denominators.
func isUSDQuote(q canonical.Asset) bool {
	return q.Type == canonical.AssetFiat && q.Code == "USD"
}

// ratFromScaledInt turns a fixed-point oracle value into an exact rational (never a float).
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

// cachedRWAReferences returns the reference snapshot, refreshed at most once per TTL window and
// shared by a single-flight gate (as in [Server.cachedRWAMembership]). On a failed read it serves
// the last good snapshot, else an UNAVAILABLE one. Unavailable is not empty: an empty snapshot
// would tell every row "no oracle publishes your instrument", a finding a failed read may not make.
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

// fillRWAReferences runs the oracle-stream read and releases the single-flight gate even on panic;
// a skipped release would leave rwaRefFlight on a channel nobody closes and wedge /v1/rwa/assets.
// It runs on its own budget because one caller's disconnect must not abort a shared fill.
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

// rwaApplyReference attaches the oracle reference and premium to one served row, or the reason
// neither is there. The refusal order is the rule order and decides the reason reported: a
// flagged issuer is answered as flagged first, since that is about the issuer, not the data.
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

	// R-0: the binding on the exact (code, issuer); a token merely sharing a ticker with a real
	// instrument cannot be handed that instrument's valuation.
	feed, bound := rwa.InstrumentFeed(a.Code, a.Issuer)
	if !bound {
		// Both refusals withhold, but the specific one is worth reporting: a token coded XAU is not
		// unbound by oversight, the oracle of that name prices a troy ounce. A fund share's own NAV
		// outranks the listing and prospectus arms.
		if rwaApplyFundNAVReference(a, snap, now) {
			return
		}
		notFound := RWAPremiumNotBound
		if rwa.OffChainReferenceCode(a.Code) {
			notFound = RWAPremiumNotInstrumentScoped
		}
		// The listing row for this exact asset, by the SAME rule /v1/assets uses ([listingEntryIn]):
		// the `CODE-GISSUER` id, then the derived SAC address. The directory lists each asset under
		// ONE form, so a classic-only key would refuse a SAC-listed member. Never the code alone;
		// both forms derive from the exact (code, issuer).
		listing, _, _ := listingEntryIn(classicListings, a.AssetID)
		// A CNAV class is valued at its prospectus NAV unless the listing price is a valuation the
		// listing arm would serve; a zero, garbage or expired row must not erase the prospectus figure.
		if _, refusal := rwaListingPrice(listing, notFound, now); refusal != "" {
			if cnav, ok := rwa.ConstantNAV(a.Code, a.Issuer); ok {
				rwaApplyConstantNAVReference(a, cnav, now)
				return
			}
		}
		// No oracle binds this pair, a statement about one source only; the address-exact listing
		// directory that prices the contract arm is read for classic rows too.
		rwaApplyListingReference(a, listing, notFound, now)
		return
	}

	// A read that did not answer knows nothing either way; only an oracle-bound row is refused on
	// it, since the unbound arm above falls through to the listing arm.
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
		// R-D: the price is served but is a declared peg or a hop through another market, so a
		// premium against it is circular.
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
// refuses), but a listing row NAMES the exact address and publishes a USD price. It applies to any
// contract row the listing names, not only ones it admitted. The figure prices the TOKEN, so the
// reference carries [RWAReferenceListingPrice] and no premium is published
// ([RWAPremiumReferenceNotOracle]) while the reference valuation is.
func rwaApplyContractReference(a *RWAAsset, entry timescale.ListingEntry, now time.Time) {
	rwaApplyListingReference(a, entry, RWAPremiumContractNotBound, now)
}

// rwaApplyListingReference is the body both arms share; `notFound` is the refusal kept when the
// directory names nothing. The classic arm reaches this only when nobody bound the pair: an
// expired, non-USD or non-positive feed is a finding about a binding this index made, and a
// listing price must not overturn it.
func rwaApplyListingReference(a *RWAAsset, entry timescale.ListingEntry, notFound string, now time.Time) {
	price, refusal := rwaListingPrice(entry, notFound, now)
	if refusal != "" {
		rwaRefuseReference(a, refusal)
		return
	}
	a.Reference = &RWAReference{
		PriceUSD: entry.PriceUSD,
		Source:   entry.Source,
		// The listing platform's own coin id, not a feed id: the field's meaning follows `provenance`.
		Feed:       entry.ListingID,
		Quote:      "fiat:USD",
		AsOf:       WireTime(entry.PricedAt),
		Stale:      now.Sub(entry.PricedAt) > rwaReferenceStaleAfter,
		Provenance: RWAReferenceListingPrice,
	}
	a.ReferenceValuation = rwaReferenceValuationOf(a, rwaReference{priceUSD: price})
	// R-C: the reference is published, the premium is not.
	a.Premium = RWAPremium{Status: RWAPremiumReferenceNotOracle}
}

// rwaListingPrice resolves a listing row to the positive price the listing arm serves, or its
// refusal. The CNAV precedence gate shares it so they cannot disagree on what counts.
func rwaListingPrice(entry timescale.ListingEntry, notFound string, now time.Time) (*big.Rat, string) {
	// An address no listing named keeps the original refusal.
	if entry.PriceUSD == "" {
		return nil, notFound
	}
	price := ratFromOptionalString(&entry.PriceUSD)
	if price == nil || price.Sign() <= 0 {
		// Not a valuation; refused rather than multiplied, as on the oracle path.
		return nil, RWAPremiumReferenceNotPositive
	}
	// The storage reader enforces the price bound in SQL; the age is re-checked here so the
	// documented bound holds however the row arrived.
	if entry.PricedAt.IsZero() || now.Sub(entry.PricedAt) > rwaReferenceMaxAge {
		return nil, RWAPremiumReferenceExpired
	}
	return price, ""
}

// rwaListingReferencesOf indexes the admitted members' listing rows by address. Built from the
// MEMBERSHIP, not a second listing read: the row that priced an asset must be the row that
// recognised it.
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
// rwaApplyConstantNAVReference prices a share class at the NAV its prospectus fixes. It is dated
// now because the value is a standing rule, not an observation; the verification date travels in
// Source. Past ReviewBy the row is served `stale: true`: labelled, not withheld.
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

// rwaFundNAVDisagrees reports whether the fund NAV bound to this row's (code, issuer), if fresher
// than [rwaFundNAVMaxAge], disagrees with the oracle value.
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

// rwaNAVDisagrees reports whether two values differ by more than [rwaNAVDisagreementTolerance].
func rwaNAVDisagrees(value, nav *big.Rat) bool {
	if value == nil || nav == nil {
		return false
	}
	diff := new(big.Rat).Sub(value, nav)
	return diff.Abs(diff).Cmp(rwaNAVDisagreementTolerance) > 0
}

// rwaApplyFundNAVReference prices a fund-share row at its fund's NAV, reporting whether it decided
// the row. A bound row with no NAV is left undecided for the lower arms; a NAV past
// [rwaFundNAVMaxAge] is refused.
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

// rwaReferenceValuationOf values one row's float at the reference price, or states why it
// cannot. It runs only on a row already carrying a published reference, so its refusals concern
// the price value and the supply, never admissibility ([rwaApplyReference]).
func rwaReferenceValuationOf(a *RWAAsset, ref rwaReference) RWAReferenceValuation {
	// A zero or negative net asset value is bad data, not a valuation of zero; the premium path
	// refuses to divide by it and this path refuses to multiply by it.
	if ref.priceUSD == nil || ref.priceUSD.Sign() <= 0 {
		return RWAReferenceValuation{Status: RWAPremiumReferenceNotPositive}
	}
	if a.CirculatingSupply == nil {
		return RWAReferenceValuation{Status: RWAReferenceValuationNoSupply}
	}
	// A contract row whose scale could not be read carries none: [RWAReferenceValuationDecimalsUnknown].
	if a.Decimals == nil {
		return RWAReferenceValuation{Status: RWAReferenceValuationDecimalsUnknown}
	}
	value := rwaReferenceValueUSD(*a.CirculatingSupply, *a.Decimals, ref.priceUSD)
	if value == "" {
		return RWAReferenceValuation{Status: RWAReferenceValuationNoSupply}
	}
	return RWAReferenceValuation{Status: RWAReferenceValuationPublished, ValueUSD: &value}
}

// rwaReferenceValueUSD = (circulating / 10^decimals) x referencePrice, as a 2-dp decimal string;
// empty on any input that is not a valuation.
//
// Exact rational arithmetic, never a float; 2-dp rounding happens once at the end so the summary
// total is the exact sum of the per-row strings. The scale is the ASSET's own decimals. A negative
// scale is refused because [big.Int.Exp] answers a negative exponent with 1, which would publish
// the raw smallest-unit count as dollars.
func rwaReferenceValueUSD(circRaw string, decimals int, unit *big.Rat) string {
	if unit == nil || decimals < 0 {
		return ""
	}
	circ := ratFromOptionalString(&circRaw)
	// A negative supply is bad data and is refused, not published as a negative valuation.
	if circ == nil || circ.Sign() < 0 {
		return ""
	}
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
	out := new(big.Rat).Quo(circ, scale)
	out.Mul(out, unit)
	return out.FloatString(2)
}

// rwaReferenceCounts totals how much of the set carries an independent valuation and how much is
// comparable, so the headline states the coverage of the comparison.
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
