package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// priceBatchMaxAssets is the upper bound on asset_ids per /v1/price/batch GET (mirrors the OpenAPI
// spec).
const priceBatchMaxAssets = 100

// priceBatchMaxAssetsPOST is the asset_ids ceiling for the POST variant, which exists to raise the
// GET limit without bloating query strings.
const priceBatchMaxAssetsPOST = 1000

// DivergenceLooker consults cached divergence results when serving /v1/price; absence and read errors both leave the warning unset.
type DivergenceLooker interface {
	// DivergenceFiringFor reports whether the divergence warning is firing for the EXACT
	// (asset, quote) pair. `checked` is true only when a cached result met the worker's own
	// min_sources_for_warning quorum, so a false `firing` with checked=false is not "prices agree".
	// The verdict is quote-specific: XLM/GBP's must never leak onto an XLM/USD response.
	// window is the aggregation window the verdict compared, 0 when the record names none.
	DivergenceFiringFor(ctx context.Context, asset, quote canonical.Asset) (firing, checked bool, window time.Duration, err error)
}

// FrozenLooker reports whether a pair's latest bucket was frozen by the anomaly checker (ADR-0019); it reports the MARKER only, and read errors fall through with frozen=false.
type FrozenLooker interface {
	FrozenForPair(ctx context.Context, asset, quote canonical.Asset) (bool, error)
}

// PriceReader is the storage-side interface for /v1/price: Redis hot path (ADR-0007) with a Timescale last-trade fallback flagged stale=true.
type PriceReader interface {
	// LatestPrice returns the most recent known price of asset in terms of quote, or
	// [ErrPriceNotFound] when the pair has no observation. sources lists the contributing
	// connectors (one for a last-trade fallback, several for VWAP); stale is true when no fresh
	// aggregated price existed and a fallback (last trade older than the freshness target) is served.
	LatestPrice(ctx context.Context, asset, quote canonical.Asset) (snapshot PriceSnapshot, sources []string, stale bool, err error)

	// RecentClosedSnapshots returns up to `n` most-recent CLOSED 1-minute VWAP snapshots, newest
	// first, for the SEP-40 `prices` passthrough. An empty slice with nil error (not
	// ErrPriceNotFound) means no closed buckets yet, so the caller can tell "unknown asset" from
	// "no history" via the asset-existence check. n is clamped to 1..200 by the caller.
	RecentClosedSnapshots(ctx context.Context, asset, quote canonical.Asset, n int) ([]PriceSnapshot, error)
}

// ErrPriceNotFound is returned by PriceReader.LatestPrice when no data exists for the pair (404).
var ErrPriceNotFound = errors.New("api: price not found for pair")

// ErrPriceWithheld means the pair has data but the thin-market substance gate refuses to publish; handlers map it to a 404 price-withheld, and the fallback chain is deliberately NOT consulted.
var ErrPriceWithheld = errors.New("api: price withheld — trailing market substance below serve floor")

// PriceWithheldReason says WHY a price was withheld (substance, scam-issuer, upstream leg); errors.Is(err, ErrPriceWithheld) still matches via Unwrap.
type PriceWithheldReason string

const (
	// PriceWithheldSubstance: the thin-market substance gate refused —
	// trailing market activity below the serve floor.
	PriceWithheldSubstance PriceWithheldReason = "substance"
	// PriceWithheldScamIssuer: a directory-flagged issuer sits on
	// either leg of the pair.
	PriceWithheldScamIssuer PriceWithheldReason = "scam_issuer"
	// PriceWithheldUpstreamLeg: a derived cross-rate's upstream leg was itself withheld.
	PriceWithheldUpstreamLeg PriceWithheldReason = "upstream_leg"
	// PriceWithheldUnattributed: a verdict that arrived without its cause; its wording claims
	// neither cause.
	PriceWithheldUnattributed PriceWithheldReason = "unattributed"
	// PriceWithheldManipulationGuard: the serving-sanity guard could not validate the bucket at
	// the requested instant ([ErrPriceAtGuarded]).
	PriceWithheldManipulationGuard PriceWithheldReason = "manipulation_guard"
	// PriceWithheldFXLeg: a closed-bucket fiat cross found no vendor FX fixing in the lookback.
	// The live rate is no substitute: the closed answer would depend on when it was read.
	PriceWithheldFXLeg PriceWithheldReason = "fx_leg_unavailable"
)

// withheldError pairs ErrPriceWithheld with the reason it fired for.
type withheldError struct {
	reason PriceWithheldReason
}

func (e *withheldError) Error() string { return ErrPriceWithheld.Error() }
func (e *withheldError) Unwrap() error { return ErrPriceWithheld }

// newPriceWithheld builds an ErrPriceWithheld-compatible error carrying
// reason, for raise sites that know WHY they are withholding.
func newPriceWithheld(reason PriceWithheldReason) error {
	return &withheldError{reason: reason}
}

// PriceWithheldError is the ErrPriceWithheld a reader returns for a pricingguard verdict, carrying which gate fired.
func PriceWithheldError(w pricingguard.Withholding) error {
	return newPriceWithheld(withheldReasonFor(w))
}

func withheldReasonFor(w pricingguard.Withholding) PriceWithheldReason {
	switch w {
	case pricingguard.WithheldFlaggedIssuer:
		return PriceWithheldScamIssuer
	case pricingguard.WithheldThinMarket:
		return PriceWithheldSubstance
	case pricingguard.NotWithheld:
	}
	return PriceWithheldUnattributed
}

// withheldBy folds the two gate seams into one verdict with pricingguard's precedence: scam gate FIRST (flagged pairs withheld
// unmeasured); a covered thin leg is released under the request's [ThinAdmission]. With scamWithheld it is the only permitted
// gate-half caller (cmd/stellarindex-api TestWithholdingGatesAreSpelledOnlyAtTheChokepoint).
func withheldBy(ctx context.Context, substance PriceSubstanceGate, scam PriceScamGate, asset, quote canonical.Asset, surface string) pricingguard.Withholding {
	if scamWithheld(ctx, scam, asset, quote, surface) {
		return pricingguard.WithheldFlaggedIssuer
	}
	if substance == nil {
		return pricingguard.NotWithheld
	}
	m, ok := substance.(PriceSubstanceMeasurer)
	if !ok {
		return pricingguard.WithholdingFor(false, substance.Allowed(ctx, asset, quote, surface))
	}
	adm := ThinAdmissionFrom(ctx)
	admit := adm.Requested() && adm.Covers(asset, quote)
	v := pricingguard.Fold(false, m.Measure(ctx, asset, quote), admit, surface)
	adm.Record(asset, quote, v)
	return v.Withholding
}

// priceWithheldReason extracts the reason err carries, or
// [PriceWithheldUnattributed] when err is the bare sentinel.
func priceWithheldReason(err error) PriceWithheldReason {
	var e *withheldError
	if errors.As(err, &e) {
		return e.reason
	}
	return PriceWithheldUnattributed
}

// PriceWithheldReasonOf is [priceWithheldReason] for producers outside this package that put a withholding on a stream.
func PriceWithheldReasonOf(err error) PriceWithheldReason { return priceWithheldReason(err) }

// PriceSubstanceGate is the serving-side thin-market gate seam, consulted directly only on paths that skip PriceReader; Allowed fails open when unmeasurable and Probe reports measured=false.
type PriceSubstanceGate interface {
	Allowed(ctx context.Context, base, quote canonical.Asset, surface string) bool
	Probe(ctx context.Context, base, quote canonical.Asset) (allowed, measured bool, floor pricingguard.SubstanceFloor)
}

// PriceSubstanceMeasurer is the optional evidence-carrying form of [PriceSubstanceGate]; a gate without it is folded through Allowed with no evidence.
type PriceSubstanceMeasurer interface {
	Measure(ctx context.Context, base, quote canonical.Asset) pricingguard.SubstanceVerdict
}

// PriceScamGate withholds the price for an asset whose ISSUER is flagged scam-class; it is the base-only half of the decision, so handlers must ask via [scamWithheld].
type PriceScamGate interface {
	Withheld(ctx context.Context, base canonical.Asset, surface string) bool
}

// PriceScamPairGate is the pair-aware [PriceScamGate]: it withholds when EITHER leg's issuer is flagged, since withholding is a property of the market, not one orientation.
type PriceScamPairGate interface {
	PriceScamGate
	WithheldPair(ctx context.Context, base, quote canonical.Asset, surface string) bool
}

// scamWithheld is the ONE spelling of the scam decision: handlers pass BOTH legs and pricingguard folds them, since per-handler
// `Withheld(base) || Withheld(quote)` drifts.
// Gates without the pair form answer base-only.
func scamWithheld(ctx context.Context, gate PriceScamGate, base, quote canonical.Asset, surface string) bool {
	if gate == nil {
		return false
	}
	if pair, ok := gate.(PriceScamPairGate); ok {
		return pair.WithheldPair(ctx, base, quote, surface)
	}
	return gate.Withheld(ctx, base, surface)
}

// writeIfScamWithheld is [scamWithheld] plus the problem write with the scam reason; a reason-free problem would tell the client the
// market is merely thin and to recompute from raw trades.
func (s *Server) writeIfScamWithheld(w http.ResponseWriter, r *http.Request, base, quote canonical.Asset, surface string) bool {
	if !scamWithheld(r.Context(), s.Scam, base, quote, surface) {
		return false
	}
	writePriceWithheldProblem(w, r, base, quote, PriceWithheldScamIssuer)
	return true
}

// writePriceWithheldProblem is the single serializer for the withheld verdict: one problem type (clients branch on it), wording by the
// gate that fired; only thin-market points at raw surfaces.
func writePriceWithheldProblem(w http.ResponseWriter, r *http.Request, asset, quote canonical.Asset, reason PriceWithheldReason) {
	writePriceWithheldProblemEvidence(w, r, asset, quote, reason, nil)
}

// writePriceWithheldProblemEvidence is [writePriceWithheldProblem] plus the substance measurement (nil: none) in the `substance` member;
// the wording never carries the numbers.
func writePriceWithheldProblemEvidence(w http.ResponseWriter, r *http.Request, asset, quote canonical.Asset, reason PriceWithheldReason, ev *SubstanceEvidence) {
	title, detail := priceWithheldWording(asset.String()+" / "+quote.String(), reason)
	if reason == "" {
		reason = PriceWithheldUnattributed
	}
	writeProblemCoverage(w, r,
		"https://api.stellarindex.io/errors/price-withheld",
		title, http.StatusNotFound, detail, nil, false, ev, reason)
}

func priceWithheldWording(pair string, reason PriceWithheldReason) (title, detail string) {
	switch reason {
	case PriceWithheldSubstance:
		return "Price withheld — market too thin to aggregate",
			"trades exist for " + pair +
				" but trailing market activity is below the serve floor, so no aggregated price is published" +
				" — read the raw market via /v1/observations, /v1/ohlc or /v1/history and apply your own judgement"
	case PriceWithheldScamIssuer:
		return "Price withheld — issuer flagged",
			"a directory-flagged issuer is on one leg of " + pair +
				", so no price is published for this market and its trades should not be used to derive one"
	case PriceWithheldUpstreamLeg:
		return "Price withheld — upstream leg withheld",
			"the price used to derive " + pair + " depends on a leg that is itself withheld, so no price is published"
	case PriceWithheldManipulationGuard:
		return "Price withheld — bucket failed the serving-sanity guard",
			"a closed bucket exists for " + pair +
				" at the requested instant, but it deviates grossly from the buckets before it (or has none to be checked against)" +
				" and no clean bucket falls within the lookback, so no price is published"
	case PriceWithheldFXLeg:
		return "Price withheld — FX leg unavailable",
			"no FX fixing binds " + pair + " at the bucket's close within the lookback, so no converted price is published" +
				" — /v1/price/tip converts at the live rate"
	case PriceWithheldUnattributed:
	}
	return "Price withheld",
		"a serving-side gate (thin market or flagged issuer) declined to publish a price for " + pair
}

// writeNoPriceProblem emits the 404 for an exhausted lookup: WITHHELD when some leg reached a withholding verdict, else plain not-found
// ("we have none" vs "we decline to publish").
func writeNoPriceProblem(w http.ResponseWriter, r *http.Request, asset, quote canonical.Asset, withheld PriceWithheldReason, ev *SubstanceEvidence) {
	if withheld != "" {
		writePriceWithheldProblemEvidence(w, r, asset, quote, withheld, ev)
		return
	}
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/price-not-found",
		"No price data for pair", http.StatusNotFound,
		"no trades or oracle observations for "+asset.String()+" / "+quote.String())
}

// defaultPriceQuote is /v1/price's implicit quote; parsed at init so dropping USD from the fiat allow-list panics instead of silently
// 400ing no-quote requests.
var defaultPriceQuote = mustParseAsset("fiat:USD")

func mustParseAsset(s string) canonical.Asset {
	a, err := canonical.ParseAsset(s)
	if err != nil {
		panic("api/v1: defaultPriceQuote " + s + " must parse: " + err.Error())
	}
	return a
}

// PriceSnapshot is the neutral shape returned by [PriceReader]; the handler wraps it in [Envelope].
type PriceSnapshot struct {
	// AssetID + Quote canonical strings match the request parameters.
	AssetID string `json:"asset_id"`
	Quote   string `json:"quote"`

	// Price as a decimal string — ADR-0003 forbids float here.
	// Computed by the reader from the underlying trade or CAGG row.
	Price string `json:"price"`

	// PriceType is one of "vwap", "twap", "last_trade", "peg"; the reader picks the best available
	// (VWAP > TWAP > last_trade). "peg" is the ONLY non-observation: the operator's standing 1:1
	// declaration, served when the direct fiat:USD read AND the XLM cross both missed.
	PriceType string `json:"price_type"`

	// ObservedAt is when the underlying trade closed (last_trade) or the aggregation-window end
	// (VWAP/TWAP). On a "peg" price nothing was observed, so it carries the declaration's
	// adoption time ([Server.declaredPegSnapshot]), not the clock. That stamp is per process and
	// resets on restart, so price_type is the durable signal.
	ObservedAt WireTime `json:"observed_at"`

	// WindowSeconds is non-zero for VWAP/TWAP — the window size.
	// Zero for last_trade.
	WindowSeconds int `json:"window_seconds,omitempty"`

	// Truncated is set only on a value served from the aggregator's
	// rolling-window cache whose coverage is known: true when the
	// window's trade read hit the row cap, so only trades after
	// CoveredFrom are all in the value. Same meaning as /v1/vwap's.
	Truncated   *bool     `json:"truncated,omitempty"`
	CoveredFrom *WireTime `json:"covered_from,omitempty"`

	// ProxyDeviation carries the proxy_deviation envelope flag from a
	// reader that does not return Flags (the tip fallbacks); never on the wire.
	ProxyDeviation bool `json:"-"`

	// FXRate is the USD→quote fixing a closed USD-anchored cross converted
	// at: the vendor bar's exact close, quote units per 1 USD.
	FXRate string `json:"fx_rate,omitempty"`
	// FXAsOf is the bound fixing's bar close, a vendor time.
	FXAsOf *WireTime `json:"fx_as_of,omitempty"`
	// FXSource is the feed that published the bound fixing.
	FXSource string `json:"fx_source,omitempty"`
	// FXResolution is "hourly" or "daily": the grain of the bound fixing.
	FXResolution string `json:"fx_resolution,omitempty"`
	// USDLeg is the USD price a closed cross converted.
	USDLeg *USDLeg `json:"usd_leg,omitempty"`

	// Change24hPct is the signed trailing-24h percentage change vs the asset's USD price ~24h ago,
	// two fractional digits ("+1.27"). Set on batch rows only when the quote is fiat:USD and a
	// closed comparison bucket exists.
	Change24hPct *string `json:"change_24h_pct,omitempty"`

	// Confidence is the multi-factor confidence score in [0, 1] (ADR-0019), populated only on
	// `/v1/price`. Nil means "not available". On a frozen response it is the held
	// last-known-good bucket's own score, kept alive for the hold.
	Confidence *float64 `json:"confidence,omitempty"`

	// ConfidenceFactors is the per-factor decomposition that
	// accompanies Confidence. Optional with the same semantics —
	// nil means "not available".
	ConfidenceFactors *ConfidenceFactors `json:"confidence_factors,omitempty"`

	// Substance is the measurement of the thin market a price was served
	// from under `include_thin=true`; absent on every other serve.
	Substance *SubstanceEvidence `json:"substance,omitempty"`

	// Substituted: the serving-sanity guard rejected the latest closed bucket and served an older
	// last-known-good one. Internal: the handler must NOT attach confidence/composite enrichment
	// read under the CURRENT tick's Redis keys (no as-of; it would misattribute a live read to a
	// stale value). See [Server.attachConfidence] / [Server.attachCompositeFlags].
	Substituted bool `json:"-"`

	// usdLeg is set only on an ADR-0051 cross ([Server.tryUSDAnchoredFiatCross]).
	usdLeg *usdLegFacts

	// cryptoCross is set only on a crypto-quoted cross ([Server.closedUSDAnchoredCryptoCross]).
	cryptoCross *cryptoCrossLegs

	// displacedBook marks an ADR-0053 basis serve: the pair's own book was read and replaced, so its
	// pair-keyed confidence and flags describe a value that is not on the wire.
	displacedBook bool
}

// USDLeg is the USD price a derived fiat price was converted from.
type USDLeg struct {
	Price      string   `json:"price"`
	ObservedAt WireTime `json:"observed_at"`
	Sources    []string `json:"sources"`
}

// usdLegFacts is what an ADR-0051 cross inherits from its USD leg. The FX rate converts one market
// observation; it is not a second market, and the pair the aggregator freezes is the USD leg.
type usdLegFacts struct {
	sources []string        // the leg's own venues, without the FX credit
	served  canonical.Asset // alias the leg's bucket was read under; zero when a fallback answered
}

// marketSingleSource is the single_source rule shared by every price surface: exactly one market
// venue stands behind the value. An FX conversion credited in sources[] is not a venue.
func marketSingleSource(snap PriceSnapshot, sources []string) bool {
	if leg := snap.usdLeg; leg != nil {
		return len(leg.sources) == 1
	}
	if c := snap.cryptoCross; c != nil {
		return len(c.asset.sources) == 1 || len(c.quote.sources) == 1
	}
	return len(sources) == 1
}

// ConfidenceFactors mirrors `confidence.Factors` on the wire so SDK consumers need no aggregate import.
type ConfidenceFactors struct {
	ZScore          float64 `json:"z_score"`
	SourceCount     float64 `json:"source_count"`
	Diversity       float64 `json:"diversity"`
	Liquidity       float64 `json:"liquidity"`
	CrossOracle     float64 `json:"cross_oracle"`
	BaselineQuality float64 `json:"baseline_quality"`

	// CrossOracleChecked is true only when real cross-oracle reference data fed the cross_oracle
	// factor. False means "could not verify" (neutral value used), NOT "references agree".
	// CrossOracleAgreement counts independent references within the divergence threshold; 0 when
	// unchecked.
	CrossOracleChecked   bool `json:"cross_oracle_checked"`
	CrossOracleAgreement int  `json:"cross_oracle_agreement"`

	// LiquidityMeasured is true when a real USD volume fed the Liquidity factor; false means the
	// neutral 0.5 was substituted because the pair couldn't be valued in USD. The neutral sits
	// inside the measured curve's range, so it is otherwise indistinguishable from mid-liquidity.
	LiquidityMeasured bool `json:"liquidity_measured"`

	// TriangulationAgreement is the seventh confidence factor: agreement between the DIRECT price
	// and the COMPOSITE its triangulation chain implies (1.0 = agrees, halving per 4pp beyond a 2%
	// tolerance, 0.7 no-data neutral). TriangulationChecked=false means no chain or no fresh
	// composite, NOT "agrees"; the factor then has weight 0.
	TriangulationAgreement float64 `json:"triangulation_agreement"`
	TriangulationChecked   bool    `json:"triangulation_checked"`

	// BaselineAgeDays is the 30d baseline DENSITY the bootstrap cap gates on, in days-equivalent
	// of 1-minute buckets (at most 30; negative = no usable baseline), not calendar age.
	// BootstrapCapped is true when the ADR-0019 ceiling (0.5) bounded confidence because density
	// is under 28.5, so 0.5 may be the cap rather than the evidence.
	BaselineAgeDays float64 `json:"baseline_age_days"`
	BootstrapCapped bool    `json:"bootstrap_capped"`
}

// ConfidenceLooker fetches the cached confidence score for /v1/price; absent entries return `(_, false, nil)` and read errors propagate without failing the request.
type ConfidenceLooker interface {
	LookupConfidence(ctx context.Context, asset, quote canonical.Asset, window time.Duration) (PriceSnapshotConfidence, bool, error)
}

// PriceSnapshotConfidence is the read-side shape between [ConfidenceLooker] and the handler, so the storage adapter decodes once.
type PriceSnapshotConfidence struct {
	Confidence float64
	Factors    ConfidenceFactors
}

// TriangulatedPriceLooker is the fallback /v1/price consults after a Timescale miss, reading the aggregator's implied VWAPs and provenance from Redis; nil leaves triangulated-only pairs 404.
type TriangulatedPriceLooker interface {
	// LookupTriangulatedVWAP returns the cached VWAP for the pair + window. found needs a value AND
	// its observed-at stamp: without one nothing says how old it is, so it is a miss. A miss is
	// not an error; err carries Redis errors for logging.
	LookupTriangulatedVWAP(ctx context.Context, base, quote canonical.Asset, window time.Duration) (vwap CachedVWAP, found bool, err error)
}

// lookupCachedVWAP is the one read of the aggregator's VWAP cache every serve path goes through. It
// refuses a value with no observation time: stamping one would be inventing it.
func (s *Server) lookupCachedVWAP(ctx context.Context, base, quote canonical.Asset, window time.Duration) (CachedVWAP, bool, error) {
	v, found, err := s.Triangulated.LookupTriangulatedVWAP(ctx, base, quote, window)
	if err != nil || !found || v.ObservedAt.IsZero() {
		return CachedVWAP{}, false, err
	}
	return v, true, nil
}

// lookupCachedVWAPAliased walks [Server.lookupCachedVWAP] over every spelling in priority order, returning the first hit and the
// literal pair it was read under: the aggregator publishes under ONE spelling (GBP composite only as crypto:XLM/fiat:GBP).
// Callers key every per-pair marker on the returned pair.
func (s *Server) lookupCachedVWAPAliased(ctx context.Context, bases, quotes []canonical.Asset, window time.Duration) (v CachedVWAP, base, quote canonical.Asset, found bool) {
	for _, a := range bases {
		for _, q := range quotes {
			if a.Equal(q) {
				continue
			}
			got, ok, err := s.lookupCachedVWAP(ctx, a, q, window)
			if err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("vwap cache lookup failed",
						"err", err, "asset", a.String(), "quote", q.String())
				}
				continue
			}
			if ok {
				return got, a, q, true
			}
		}
	}
	return CachedVWAP{}, canonical.Asset{}, canonical.Asset{}, false
}

// CompositeMetaLooker is an OPTIONAL [TriangulatedPriceLooker] capability exposing `diverged`/`rerouted` flags; a miss or error leaves them unset and NEVER fails the request.
type CompositeMetaLooker interface {
	// LookupCompositeMeta returns the raw JSON blob under [cachekeys.VWAPCompositeMeta]; a miss is
	// found=false, and err is a real read failure for logging only.
	LookupCompositeMeta(ctx context.Context, base, quote canonical.Asset, window time.Duration) (raw []byte, found bool, err error)
}

// ─── Handler ──────────────────────────────────────────────────────

// resolveAssetOrBaseParam reads `asset` or its alias `base` (so /v1/twap URLs work); both given is a 400. Dual of `parseBaseQuote` in
// history.go; ok=false means a 400 was written.
func resolveAssetOrBaseParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	rawAsset := r.URL.Query().Get("asset")
	rawBase := r.URL.Query().Get("base")
	if rawAsset != "" && rawBase != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-parameter",
			"`asset` and `base` are mutually exclusive", http.StatusBadRequest,
			"both query parameters refer to the same value — pick one (this endpoint's canonical form is `asset=`; `base=` is accepted as an alias for /v1/twap compatibility)")
		return "", false
	}
	if rawAsset == "" {
		rawAsset = rawBase
	}
	if rawAsset == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-asset",
			"Missing asset parameter", http.StatusBadRequest,
			"asset query parameter is required (or `base=` as an alias for /v1/twap compatibility)")
		return "", false
	}
	return rawAsset, true
}

// parsePriceAssetParam delegates to the shared `asset=`-canonical resolver.
func parsePriceAssetParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	return resolveAssetOrBaseParam(w, r)
}

// parsePricePairParams is handlePrice's parameter prologue (asset=/base= alias, quote default, identity-pair rejection); ok=false means a
// problem+json was written.
func (s *Server) parsePricePairParams(w http.ResponseWriter, r *http.Request) (asset, quote canonical.Asset, ok bool) {
	rawAsset, ok := parsePriceAssetParam(w, r)
	if !ok {
		return canonical.Asset{}, canonical.Asset{}, false
	}
	asset, err := canonical.ParseAsset(rawAsset)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset identifier", http.StatusBadRequest,
			err.Error())
		return canonical.Asset{}, canonical.Asset{}, false
	}

	quote, ok = parsePriceQuoteParam(w, r)
	if !ok {
		return canonical.Asset{}, canonical.Asset{}, false
	}

	if asset.Equal(quote) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/identity-price",
			"Asset and quote are the same", http.StatusBadRequest,
			"price of an asset in itself is always 1; parameters must differ")
		return canonical.Asset{}, canonical.Asset{}, false
	}
	return asset, quote, true
}

// handlePrice serves GET /v1/price?asset=&quote= (quote defaults to fiat:USD): the last CLOSED 1m VWAP bucket (ADR-0015), byte-identical
// across regions; ?window= is rejected (ADR-0018).
// flags.stale means a last-trade fallback, a frozen pair's held value ([Server.resolveFrozenServe]), or an older bucket held in place of
// an outlier.
func (s *Server) handlePrice(w http.ResponseWriter, r *http.Request) {
	// Rejected before the reader-nil check so a misconfigured deployment 400s like a healthy one.
	if rawWindow := r.URL.Query().Get("window"); rawWindow != "" && rawWindow != "60" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/window-not-supported",
			"window is not a supported parameter", http.StatusBadRequest,
			"/v1/price always serves the closed 1-minute bucket (ADR-0015); rolling windows are /v1/price/tip's job")
		return
	}

	reader := s.Prices
	if reader == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Price serving not configured", http.StatusServiceUnavailable,
			"this deployment has no PriceReader wired — check binary configuration")
		return
	}

	asset, quote, ok := s.parsePricePairParams(w, r)
	if !ok {
		return
	}

	ps, adm := s.resolvePriceServeThin(r, reader, asset, quote)
	// Withheld beats every fallback, which would re-serve the same substanceless market (see
	// ErrPriceWithheld).
	if errors.Is(ps.err, ErrPriceWithheld) {
		reason := priceWithheldReason(ps.err)
		writePriceWithheldProblemEvidence(w, r, asset, quote, reason, thinEvidenceFor(adm, reason))
		return
	}
	if ps.miss != nil {
		s.writeFallbackMiss(w, r, asset, quote, *ps.miss, thinEvidenceFor(adm, ps.miss.withheld))
		return
	}
	if err := ps.err; err != nil {
		if clientAborted(r, err) {
			return // middleware labels request as 499
		}
		s.logger.Error("LatestPrice failed",
			"err", err,
			"asset", asset.String(),
			"quote", quote.String())
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	snapshot, sources, stale, served, triangulated, viaFallback := ps.snap, ps.sources, ps.stale, ps.served, ps.triangulated, ps.viaFallback
	if !viaFallback {
		if basis, ok := s.preferUSDAnchoredBasis(r.Context(), asset, quote, snapshot, sources); ok {
			snapshot, sources, served, triangulated, stale = basis.snap, basis.sources, basis.served, true, basis.stale
			viaFallback = true
		}
	}

	// A frozen pair serves the value the freeze is HOLDING (see [Server.resolveFrozenServe]). It
	// comes from the VWAP cache, so it is viaFallback: already normalized, and stale.
	held := s.resolveFrozenServeFor(r, snapshot, asset, served, quote)
	if held.outcome == frozenServeNothingHeld {
		writeFrozenNothingHeldProblem(w, r, asset, quote)
		return
	}
	frozen := held.outcome == frozenServeHeld
	frozenChecked := held.checked
	if frozen {
		held.snapshot.displacedBook = snapshot.displacedBook
		snapshot, sources, triangulated = held.snapshot, held.sources, held.triangulated
		stale, viaFallback = true, true
		served = held.pairBase
	}

	s.handlePriceTail(w, r, asset, quote, served, snapshot, sources, stale, triangulated, viaFallback, frozen, frozenChecked, adm)
}

// priceServe is one pass of /v1/price's read: closed-bucket read, then the fallback chain on a
// not-found. Exactly one of a price, miss or err (ErrPriceWithheld if withheld) is set.
type priceServe struct {
	snap         PriceSnapshot
	sources      []string
	stale        bool
	served       canonical.Asset
	triangulated bool
	// viaFallback: the value came from priceFallback, already normalized
	// upstream, so normalizeRawPriceSnapshot must not run on it a second time.
	viaFallback bool
	miss        *fallbackResult // the fallback chain served nothing
	err         error
}

// retryReason is the withheld reason a thin second pass keys on.
func (p priceServe) retryReason() PriceWithheldReason {
	switch {
	case p.miss != nil:
		return p.miss.withheld
	case errors.Is(p.err, ErrPriceWithheld):
		return priceWithheldReason(p.err)
	default:
		return ""
	}
}

func (p priceServe) priced() bool {
	return p.miss == nil && p.err == nil
}

// resolvePriceServe runs one pass under whatever [ThinAdmission] ctx carries.
func (s *Server) resolvePriceServe(ctx context.Context, reader PriceReader, asset, quote canonical.Asset) priceServe {
	// Aliases are tried because the aggregator writes VWAP under whichever XLM form its pair set
	// names (ADR-0010).
	snapshot, sources, stale, served, err := s.readPriceWithAliasesServed(ctx, reader, asset, quote)
	if errors.Is(err, ErrPriceNotFound) {
		fb := s.priceFallback(ctx, asset, quote)
		if !fb.ok {
			return priceServe{miss: &fb}
		}
		// Fallbacks sit below the closed-bucket contract, so stale is the chain's own verdict
		// (a closed fiat cross reports its own legs').
		return priceServe{
			snap: fb.snap, sources: fb.sources, stale: fb.stale, served: fb.served,
			triangulated: fb.triangulated, viaFallback: true,
		}
	}
	if err != nil {
		return priceServe{err: err}
	}
	return priceServe{snap: snapshot, sources: sources, stale: stale, served: served}
}

// resolvePriceServeThin is the two-pass driver: the default read, then (only if it ended withheld on a measured thin leg and the
// request opted in) again with that leg admitted, so a cleared route always beats a thin one. The returned record is the served
// pass's.
func (s *Server) resolvePriceServeThin(r *http.Request, reader PriceReader, asset, quote canonical.Asset) (priceServe, *ThinAdmission) {
	offCtx, off := WithThinAdmission(r.Context(), asset, quote, false)
	ps := s.resolvePriceServe(offCtx, reader, asset, quote)
	if ps.priced() || !includeThinRequested(r) || !off.ThinWithheld(ps.retryReason()) {
		return ps, off
	}
	onCtx, on := WithThinAdmission(r.Context(), asset, quote, true)
	thin := s.resolvePriceServe(onCtx, reader, asset, quote)
	switch {
	case thin.err != nil && !errors.Is(thin.err, ErrPriceWithheld):
		s.logger.Warn("include_thin second pass failed; serving the withheld verdict",
			"err", thin.err, "asset", asset.String(), "quote", quote.String())
	case thin.miss != nil && thin.miss.err != nil:
		s.logger.Warn("include_thin second pass failed; serving the withheld verdict",
			"err", thin.miss.err, "asset", asset.String(), "quote", quote.String())
	case thin.priced() && thin.snap.Price != "":
		return thin, on
	}
	return ps, off
}

// handlePriceTail continues handlePrice (normalization, confidence/flags, response write), split
// out for the funlen ceiling.
func (s *Server) handlePriceTail(w http.ResponseWriter, r *http.Request, asset, quote, served canonical.Asset, snapshot PriceSnapshot, sources []string, stale, triangulated, viaFallback, frozen, frozenChecked bool, adm *ThinAdmission) {
	// Nonstandard-decimals normalization applies only to the RAW prices_1m read: priceFallback's
	// Redis-VWAP branch is already normalized upstream and its peg/cross branches never touch a
	// flagged Soroban leg.
	if !viaFallback {
		s.normalizeRawPriceSnapshot(&snapshot, asset, quote)
	}

	// No obs.PriceStalenessSeconds here: one series per queried asset is a cardinality blowup.
	// Confidence is best-effort and skipped on a substituted snapshot or displaced book, whose pair
	// keys describe the replaced value.
	pairKeyed := !snapshot.Substituted && !snapshot.displacedBook
	if pairKeyed {
		s.attachConfidence(r, &snapshot, asset, quote, heldWindow(frozen, snapshot, confidenceLookupWindow))
	}

	// Per-pair markers are keyed on the literal pair the aggregator priced, so ask under the served
	// spelling, not the client's.
	governing := served
	if governing.IsZero() {
		governing = asset
	}
	flags := Flags{Stale: stale, Triangulated: triangulated, ProxyDeviation: triangulated && s.proxyDeviation(r.Context(), time.Now().UTC())}
	// Composite-quality signals (diverged / rerouted) run whichever arm served, since the router
	// tracks the chain even when a closed bucket wins.
	if pairKeyed {
		s.attachCompositeFlags(r, &flags, governing, quote, heldWindow(frozen, snapshot, triangulationLookupWindow), false)
	}
	flags.Frozen = frozen
	flags.FrozenChecked = frozenChecked
	flags.Degraded = frozen
	// SingleSource is forced true on the LKG fallback (a multi-source bucket couldn't have been
	// frozen). If a WIRED freeze looker's read failed, freeze status is UNKNOWN, so deriving it
	// would assert a fact the read never established.
	freezeReadFailed := s.Freeze != nil && !frozenChecked
	switch {
	case frozen:
		flags.SingleSource = true
	case freezeReadFailed:
		flags.SingleSource = false
	default:
		flags.SingleSource = marketSingleSource(snapshot, sources)
	}
	if pairKeyed {
		flags.DivergenceWarning, flags.DivergenceChecked = s.lookupDivergenceFlag(r.Context(), governing, quote, 0)
	}
	if adm.Admitted() && snapshot.Price != "" {
		capThinConfidence(&snapshot)
		flags.ThinMarket = true
		snapshot.Substance = substanceEvidenceWire(adm.Evidence())
	}
	writeJSON(w, snapshot, flags, sources...)
}

// confidenceLookupWindow is the cache window for `/v1/price`'s confidence lookup: the smallest in
// `orchestrator.DefaultWindows` (5m), the freshest cached score, so a stale one TTLs out unread.
const confidenceLookupWindow = 5 * time.Minute

// triangulationLookupWindow is the Redis VWAP window read: 5m is the smallest ([orchestrator.DefaultWindows]), no writer emits
// 1m. Served `observed_at` is the aggregator's stamp ([cachekeys.VWAPObservedAt]), not read time, since a freeze keeps a value
// cached unrewritten for its whole hold.
const triangulationLookupWindow = 5 * time.Minute

// tryRedisVWAPFallback consults the [TriangulatedPriceLooker] after a Timescale miss, covering triangulated and stablecoin-proxy
// values absent from `prices_1m` (literal trade pairs only); marker-absent hits are accepted. Sources is empty (no provenance in
// Redis keys); staleness is the caller's call (ADR-0018).
func (s *Server) tryRedisVWAPFallback(ctx context.Context, asset, quote canonical.Asset) (snap PriceSnapshot, sources []string, served canonical.Asset, triangulated, ok bool) {
	if s.Triangulated == nil {
		return PriceSnapshot{}, nil, canonical.Asset{}, false, false
	}
	v, served, _, found := s.lookupCachedVWAPAliased(ctx, assetAliases(asset), []canonical.Asset{quote}, triangulationLookupWindow)
	if !found {
		return PriceSnapshot{}, nil, canonical.Asset{}, false, false
	}
	snap = withCoverage(PriceSnapshot{
		AssetID:       asset.String(),
		Quote:         quote.String(),
		Price:         v.Value,
		PriceType:     "vwap",
		ObservedAt:    WireTime(v.ObservedAt),
		WindowSeconds: int(triangulationLookupWindow.Seconds()),
	}, v.Coverage)
	return snap, []string{}, served, v.Triangulated, true
}

// assetAliases is the api/v1 spelling of [canonical.AssetAliases] (`native`, `crypto:XLM`, its SAC) in read priority; SAC last so a thin
// Soroban pool cannot outrank SDEX + CEX depth.
func assetAliases(asset canonical.Asset) []canonical.Asset {
	return canonical.AssetAliases(asset)
}

// distinctMarkets drops any pair whose flip already appeared (first spelling wins, classic before SAC):
// [timescale.Store.TradesInRange] and OHLCSeries read a market in BOTH stored directions, so a pair merged with its flip would
// count every trade twice (e.g. `?asset=native&quote=crypto:XLM` in [tipMergePairs]).
func distinctMarkets(pairs []canonical.Pair) []canonical.Pair {
	if len(pairs) < 2 {
		return pairs
	}
	out := make([]canonical.Pair, 0, len(pairs))
	for _, p := range pairs {
		dup := false
		for _, kept := range out {
			if kept.EqualEitherWay(p) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

// sameAsset reports whether two forms name the SAME asset (one alias family, on the [canonical.CanonicalAsset] fold key); a proxy walk's
// self-pair has no rows and wastes shared deadline.
func sameAsset(a, b canonical.Asset) bool {
	return canonical.CanonicalAsset(a).Equal(canonical.CanonicalAsset(b))
}

// usdPegProxyQuotes is the `[trades].usd_pegged_classic_assets` walk order with every canonical form of each peg folded in, deduplicated
// (a peg is an ASSET: Soroban AMMs trade the SAC).
// Every classic peg stays ahead of every SAC form, or peg A's thin Soroban pool would be read before peg B's deep classic book.
func (s *Server) usdPegProxyQuotes() []canonical.Asset {
	out := make([]canonical.Asset, 0, 2*len(s.USDPeggedClassics))
	seen := make(map[string]struct{}, 2*len(s.USDPeggedClassics))
	add := func(form canonical.Asset) {
		k := form.String()
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		out = append(out, form)
	}
	// Pass 1: each peg's priority-first (classic) form; pass 2: the rest, SAC last.
	for _, peg := range s.USDPeggedClassics {
		add(canonical.CanonicalAsset(peg))
	}
	for _, peg := range s.USDPeggedClassics {
		for _, form := range assetAliases(peg) {
			add(form)
		}
	}
	return out
}

// echoRequestedAsset restamps AssetID with the asset the CLIENT asked for, not the alias the store was keyed under, or asking for
// both `native` and `crypto:XLM` would return two identical ids to batch clients ([VWAP1mToSnapshot]'s contract).
func echoRequestedAsset(snap PriceSnapshot, requested canonical.Asset) PriceSnapshot {
	snap.AssetID = requested.String()
	return snap
}

// readPriceWithAliases is [Server.readPriceWithAliasesServed] without the served alias: first fresh alias hit, else first stale, else
// first withheld error, else first error.
// A bare LatestPrice(native, ...) would miss the `crypto:XLM` VWAP that CEX trades fill.
func (s *Server) readPriceWithAliases(ctx context.Context, reader PriceReader, asset, quote canonical.Asset) (PriceSnapshot, []string, bool, error) {
	snap, srcs, stale, _, err := s.readPriceWithAliasesServed(ctx, reader, asset, quote)
	return snap, srcs, stale, err
}

// priceReadResult carries readPriceWithAliasesServedOnce's results through singleflight, which
// returns one value.
type priceReadResult struct {
	snap   PriceSnapshot
	srcs   []string
	stale  bool
	served canonical.Asset
	// thinAdmitted and evidence are the flight's own [ThinAdmission] answer, set on the error path
	// too so a 404 body can carry evidence.
	thinAdmitted bool
	evidence     *pricingguard.SubstanceEvidence
}

// priceReadFlightTimeout bounds a coalesced price read. It runs on a context detached from any
// caller so one caller's cancellation can't cut the read short for the others sharing the key.
const priceReadFlightTimeout = 8 * time.Second

// readPriceWithAliasesServed coalesces [Server.readPriceWithAliasesServedOnce] via singleflight per (asset, quote). ctx is read only for
// the caller's [ThinAdmission];
// that opt-in bit is part of the flight key, so a default caller never shares a flight that released a thin market.
func (s *Server) readPriceWithAliasesServed(ctx context.Context, reader PriceReader, asset, quote canonical.Asset) (PriceSnapshot, []string, bool, canonical.Asset, error) {
	adm := ThinAdmissionFrom(ctx)
	admit := adm.Requested() && adm.Covers(asset, quote)
	key := asset.String() + "/" + quote.String() + "/admit:" + strconv.FormatBool(admit)
	v, err, _ := s.priceReadFlight.Do(key, func() (any, error) { //nolint:contextcheck // singleflight deliberately survives per-caller cancellation, see internal/metadata/cache.go
		fetchCtx, cancel := context.WithTimeout(context.Background(), priceReadFlightTimeout)
		defer cancel()
		fetchCtx, own := WithThinAdmission(fetchCtx, asset, quote, admit)
		snap, srcs, stale, served, err := s.readPriceWithAliasesServedOnce(fetchCtx, reader, asset, quote)
		res := priceReadResult{thinAdmitted: own.Admitted(), evidence: own.Evidence()}
		if err != nil {
			return res, err
		}
		res.snap, res.srcs, res.stale, res.served = snap, srcs, stale, served
		return res, nil
	})
	r, _ := v.(priceReadResult)
	if adm.Covers(asset, quote) {
		adm.Merge(r.thinAdmitted, r.evidence)
	}
	if err != nil {
		return PriceSnapshot{}, nil, false, canonical.Asset{}, err
	}
	return r.snap, r.srcs, r.stale, r.served, nil
}

// readPriceWithAliasesServedOnce is [Server.readPriceWithAliases] plus WHICH alias was read: the freeze marker is keyed on the
// literal pair the aggregator prices ([Server.resolveFrozenServe]). The served alias is zero when err != nil.
func (s *Server) readPriceWithAliasesServedOnce(ctx context.Context, reader PriceReader, asset, quote canonical.Asset) (PriceSnapshot, []string, bool, canonical.Asset, error) {
	aliases := assetAliases(asset)
	var firstSnap PriceSnapshot
	var firstSrcs []string
	var firstServed canonical.Asset
	var firstErr error
	var firstWithheldErr error
	freshFound := false
	for _, a := range aliases {
		snap, srcs, stale, err := reader.LatestPrice(ctx, a, quote)
		if err != nil {
			if errors.Is(err, ErrPriceWithheld) && firstWithheldErr == nil {
				// Keep the reader's own error so its withheld reason survives to the response.
				firstWithheldErr = err
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !stale {
			// Fresh hit — return immediately.
			return echoRequestedAsset(snap, asset), srcs, false, a, nil
		}
		// Remember the first stale result in case every alias is stale.
		if !freshFound {
			firstSnap, firstSrcs, firstServed = echoRequestedAsset(snap, asset), srcs, a
			freshFound = true
		}
	}
	if freshFound {
		// All stale: still surface flags.stale=true rather than fall through to priceFallback.
		return firstSnap, firstSrcs, true, firstServed, nil
	}
	// A withheld verdict on ANY alias wins over not-found, which would send the caller into
	// priceFallback and re-serve the withheld market through the Redis/proxy side doors.
	if firstWithheldErr != nil {
		return PriceSnapshot{}, nil, false, canonical.Asset{}, firstWithheldErr
	}
	// Otherwise the first error keeps the ErrPriceNotFound -> priceFallback branch working.
	return PriceSnapshot{}, nil, false, canonical.Asset{}, firstErr
}

// normalizeRawPriceSnapshot applies nonstandard-decimals normalization (aggregate.AdjustPrice) to a snapshot from a RAW prices_1m ratio
// (exact scalar multiply);
// never on a priceFallback snapshot, already normalized. A byte-identical no-op when baseDecimals == quoteDecimals: reformatting would
// change wire bytes for 7dp pairs.
func (s *Server) normalizeRawPriceSnapshot(snap *PriceSnapshot, base, quote canonical.Asset) {
	snap.Price = s.normalizeRawRatioString(snap.Price, base, quote)
}

// NormalizeRawPriceSnapshot is [Server.normalizeRawPriceSnapshot] for callers with no *Server; nil decimals fails open at aggregate.StandardDecimals.
func NormalizeRawPriceSnapshot(snap *PriceSnapshot, base, quote canonical.Asset, decimals *NonstandardDecimalsCache) {
	snap.Price = normalizeRawRatioStringWithLookup(snap.Price, base, quote, decimals)
}

// normalizeRawRatioString is the string-level primitive for raw-ratio paths (/v1/price/at, /v1/price/changes); base/quote MUST be
// the ACTUAL legs priced (asset/<peg> for a stablecoin-proxy read, NOT fiat:USD). Returns value byte-identical when decimals
// match or it doesn't parse.
func (s *Server) normalizeRawRatioString(value string, base, quote canonical.Asset) string {
	return normalizeRawRatioStringWithLookup(value, base, quote, s.NonstandardDecimals)
}

// normalizeRawRatioStringWithLookup is the one implementation under [Server.normalizeRawRatioString]
// and [NormalizeRawPriceSnapshot].
func normalizeRawRatioStringWithLookup(value string, base, quote canonical.Asset, decimals *NonstandardDecimalsCache) string {
	baseDec := aggregate.ResolveDecimals(decimals, base)
	quoteDec := aggregate.ResolveDecimals(decimals, quote)
	if baseDec == quoteDec {
		return value
	}
	raw := ratFromDecimal(value)
	if raw == nil {
		return value
	}
	return ratToDecimal(aggregate.AdjustPrice(raw, baseDec, quoteDec), ohlcPriceDigits)
}

// priceFallback runs the post-Timescale-miss chain for /v1/price: (1) Redis VWAP cache, (2) read-time stablecoin -> fiat:USD
// rewrite against the declared classic USD-pegs, (3) fiat-vs-fiat cross-rate from the forex snapshot (always triangulated).
// ok=false when all miss; served is layer 1's base alias, else zero. The chain is entered on ErrPriceNotFound, whose exits are
// UNGATED, so the scam decision is asked HERE.
func (s *Server) priceFallback(ctx context.Context, asset, quote canonical.Asset) fallbackResult {
	// Scam gate before layer 1: the cached VWAP is written without directory consultation. Later
	// layers read through the GATED LatestPrice, so one ask here covers all. SCAM ONLY, not the
	// substance gate: that floor is measured on the alias union, and a triangulated pair has zero
	// literal rows BY CONSTRUCTION, so it would withhold the whole triangulated long tail.
	if scamWithheld(ctx, s.Scam, asset, quote, "price_read") {
		return fallbackResult{withheld: PriceWithheldScamIssuer}
	}
	if snap, srcs, served, triangulated, ok := s.tryRedisVWAPFallback(ctx, asset, quote); ok {
		return fallbackResult{snap: snap, sources: srcs, served: served, triangulated: triangulated, ok: true, stale: true}
	}
	snap, srcs, ok, withheld := s.tryStablecoinFiatProxy(ctx, asset, quote)
	if ok {
		return fallbackResult{snap: snap, sources: srcs, triangulated: true, ok: true, stale: true}
	}
	// Last, crosses: fiat/fiat, USD-anchored for a non-fiat asset quoted in a fiat with no market
	// (ADR-0051), or through USD for one quoted in a crypto ticker. A direct book is weighed by
	// [Server.preferUSDAnchoredBasis].
	var cross fallbackResult
	switch {
	case asset.Type == canonical.AssetFiat:
		cross = s.closedFiatCross(ctx, asset, quote)
	case quote.Type == canonical.AssetCrypto:
		cross = s.closedUSDAnchoredCryptoCross(ctx, asset, quote)
	default:
		cross = s.closedUSDAnchoredFiatCross(ctx, asset, quote)
	}
	if cross.ok || cross.err != nil || cross.withheld != "" {
		return cross
	}
	// `withheld` separates "no price" from "one we decline to publish" (price-withheld vs not-found).
	if withheld != "" {
		return fallbackResult{withheld: withheld}
	}
	return fallbackResult{}
}

// fallbackResult is one run of [Server.priceFallback].
type fallbackResult struct {
	snap         PriceSnapshot
	sources      []string
	served       canonical.Asset
	triangulated bool
	ok           bool
	// stale is flags.stale: fallbacks sit below the closed-bucket contract, except a closed fiat
	// cross, which carries its legs' own staleness.
	stale    bool
	withheld PriceWithheldReason // non-empty: a leg declined to publish
	err      error               // the FX store failed: not a miss
}

// writeFallbackMiss answers an exhausted fallback chain: 503 when the FX
// store failed, else the withheld or not-found 404 carrying ev.
func (s *Server) writeFallbackMiss(w http.ResponseWriter, r *http.Request, asset, quote canonical.Asset, fb fallbackResult, ev *SubstanceEvidence) {
	if fb.err != nil {
		if clientAborted(r, fb.err) {
			return
		}
		s.logger.Error("fx fixing read failed", "err", fb.err, "asset", asset.String(), "quote", quote.String())
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"FX rate unavailable", http.StatusServiceUnavailable,
			"the FX fixing store could not be read; retry")
		return
	}
	writeNoPriceProblem(w, r, asset, quote, fb.withheld, ev)
}

// fxBindFailure maps a failed [Server.bindFXFixings] to its fallback
// outcome: a lookback miss withholds, anything else is a store failure.
func fxBindFailure(err error) (fallbackResult, bool) {
	switch {
	case err == nil:
		return fallbackResult{}, false
	case errors.Is(err, errFXLegMissing):
		return fallbackResult{withheld: PriceWithheldFXLeg}, true
	}
	return fallbackResult{err: err}, true
}

// basisMinUSDVenues is how many venues the USD leg needs to displace a single-venue fiat book.
const basisMinUSDVenues = 2

// preferUSDAnchoredBasis is the ADR-0053 basis rule for a direct non-fiat/fiat snapshot: a single-venue book yields to the
// USD-anchored derivation when that is fresh and rests on a multi-venue USD leg. ok=false keeps the direct market whenever the
// derivation misses, withholds or fails, so the rule never costs a price.
func (s *Server) preferUSDAnchoredBasis(ctx context.Context, asset, quote canonical.Asset, direct PriceSnapshot, directSources []string) (fallbackResult, bool) {
	if s.DisableFiatBasis || !marketSingleSource(direct, directSources) {
		return fallbackResult{}, false
	}
	cross := s.closedUSDAnchoredFiatCross(ctx, asset, quote)
	if !cross.ok || cross.stale || cross.snap.usdLeg == nil || len(cross.snap.usdLeg.sources) < basisMinUSDVenues {
		return fallbackResult{}, false
	}
	cross.snap.displacedBook = true
	return cross, true
}

// convertAtFixing composes a USD price with a bound fixing in exact rationals (invariant 1).
// observed_at stays the USD leg's; the fixing's own time is fx_as_of.
func convertAtFixing(usd PriceSnapshot, usdSources []string, quote canonical.Asset, b timescale.FXFixingBinding) (PriceSnapshot, bool) {
	usdRat, okU := new(big.Rat).SetString(usd.Price)
	rate, okR := fixingRate(b)
	if !okU || !okR {
		return PriceSnapshot{}, false
	}
	if usdSources == nil {
		usdSources = []string{}
	}
	out := usd
	out.Quote = quote.String()
	out.Price = formatCrossRate(new(big.Rat).Mul(usdRat, rate))
	asOf := WireTime(b.BarEnd)
	out.FXRate, out.FXAsOf, out.FXSource, out.FXResolution = b.RateUSD, &asOf, b.Source, b.Resolution
	out.USDLeg = &USDLeg{Price: usd.Price, ObservedAt: usd.ObservedAt, Sources: usdSources}
	return out, true
}

// closedFiatCross is [Server.tryFiatCrossRate] for the closed surfaces: both legs bind to fixings
// at the current minute, so every region agrees until a bound bar changes.
func (s *Server) closedFiatCross(ctx context.Context, asset, quote canonical.Asset) fallbackResult {
	if asset.Type != canonical.AssetFiat || quote.Type != canonical.AssetFiat || s.fxFixings == nil {
		return fallbackResult{}
	}
	e := time.Now().UTC().Truncate(time.Minute)
	tickers := make([]string, 0, 2)
	for _, code := range []string{asset.Code, quote.Code} {
		if code != "USD" {
			tickers = append(tickers, code)
		}
	}
	bound, err := s.bindFXFixings(ctx, tickers, e)
	if miss, done := fxBindFailure(err); done {
		return miss
	}
	rateOf := func(code string) (*big.Rat, bool) {
		if code == "USD" {
			return big.NewRat(1, 1), true
		}
		return fixingRate(bound[code])
	}
	rateAsset, okA := rateOf(asset.Code)
	rateQuote, okQ := rateOf(quote.Code)
	if !okA || !okQ {
		return fallbackResult{withheld: PriceWithheldFXLeg}
	}
	var sources []string
	var asOf time.Time
	resolution, stale := timescale.FXResolutionHourly, false
	for _, t := range tickers {
		b := bound[t]
		sources = appendFXSource(sources, b.Source)
		if asOf.IsZero() || b.BarEnd.Before(asOf) {
			asOf = b.BarEnd
		}
		if b.Resolution == timescale.FXResolutionDaily {
			resolution = timescale.FXResolutionDaily
		}
		stale = stale || fxFixingStale(e, b)
	}
	wireAsOf := WireTime(asOf)
	return fallbackResult{
		snap: PriceSnapshot{
			AssetID:      asset.String(),
			Quote:        quote.String(),
			Price:        formatCrossRate(new(big.Rat).Quo(rateQuote, rateAsset)),
			PriceType:    "vwap",
			ObservedAt:   wireAsOf,
			FXAsOf:       &wireAsOf,
			FXResolution: resolution,
		},
		sources: sources, triangulated: true, ok: true, stale: stale,
	}
}

// tryUSDAnchoredFiatCross prices a NON-fiat asset in any fiat with an FX rate: price(asset, USD) x rate_usd[CCY] (ADR-0051); the tip's
// arm (closed surfaces use [Server.closedUSDAnchoredFiatCross]).
// An FX leg older than [Server.fxCrossMaxAge] is refused. `withheld` is load-bearing: withholding is decided on the USD leg, so an FX
// rate onto a withheld leg would publish it via an ungated side door.
func (s *Server) tryUSDAnchoredFiatCross(
	ctx context.Context, asset, quote canonical.Asset,
) (PriceSnapshot, []string, bool, bool) {
	if asset.Type == canonical.AssetFiat || quote.Type != canonical.AssetFiat {
		return PriceSnapshot{}, nil, false, false
	}
	if quote.Code == "USD" {
		return PriceSnapshot{}, nil, false, false
	}
	if s.Currencies == nil {
		return PriceSnapshot{}, nil, false, false
	}
	fx := s.Currencies.Latest()
	if fx == nil {
		return PriceSnapshot{}, nil, false, false
	}
	var rate float64
	var rateUpdatedAt time.Time
	var rateSource string
	for _, c := range fx.Currencies {
		if c.Ticker == quote.Code {
			rate = c.RateUSD
			rateUpdatedAt = c.UpdatedAt
			rateSource = fxSourceOf(c)
			break
		}
	}
	if rate <= 0 {
		return PriceSnapshot{}, nil, false, false
	}
	// fxObservedAt: the older of snapshot publication and this ticker's UpdatedAt. [Cache.Latest]
	// never expires, so without this bound an fx worker outage would serve the last fetch forever.
	fxObservedAt := olderNonZero(fx.PublishedAt, rateUpdatedAt)
	if fxCrossStale(fxObservedAt, s.fxCrossMaxAge) {
		return PriceSnapshot{}, nil, false, false
	}

	usdSnap, usdSources, usdServed, _, ok, withheld := s.resolveUSDLeg(ctx, asset)
	if withheld {
		return PriceSnapshot{}, nil, false, true
	}
	// A declared peg is 1:1 against USD only; times a rate it is neither declaration nor observation.
	if !ok || isDeclaredPeg(usdSnap) {
		return PriceSnapshot{}, nil, false, false
	}

	// Exact rationals (ADR-0003); the float64 rate goes via its shortest round-trip decimal.
	usdRat, okU := new(big.Rat).SetString(usdSnap.Price)
	rateRat, okR := new(big.Rat).SetString(strconv.FormatFloat(rate, 'f', -1, 64))
	if !okU || !okR {
		return PriceSnapshot{}, nil, false, false
	}

	out := usdSnap
	out.AssetID = asset.String()
	out.Quote = quote.String()
	out.Price = formatCrossRate(new(big.Rat).Mul(usdRat, rateRat))
	// observed_at is the OLDER leg: the USD stamp alone would hide a stale FX component.
	out.ObservedAt = WireTime(olderNonZero(time.Time(usdSnap.ObservedAt), fxObservedAt))
	out.usdLeg = &usdLegFacts{sources: usdSources, served: usdServed}
	return out, appendFXSource(usdSources, rateSource), true, false
}

// defaultFXCrossMaxAge is the default [Server.fxCrossMaxAge]. It mirrors
// orchestrator.DefaultCompositeReferenceFXMaxAge (76h): daily buckets that pause over market closes.
const defaultFXCrossMaxAge = 76 * time.Hour

// fxCrossMaxAgeOrDefault resolves pricing_guard.fx_cross_max_age_hours, defaulting when unset.
func fxCrossMaxAgeOrDefault(hours int) time.Duration {
	if hours <= 0 {
		return defaultFXCrossMaxAge
	}
	return time.Duration(hours) * time.Hour
}

// fxCrossStale reports whether an FX leg is too old to serve (gate shared by [Server.tryFiatCrossRate] and
// [Server.tryUSDAnchoredFiatCross]); a zero observedAt is unknown, so stale.
func fxCrossStale(observedAt time.Time, maxAge time.Duration) bool {
	if observedAt.IsZero() {
		return true
	}
	return time.Since(observedAt) > maxAge
}

// resolveUSDLeg resolves asset/fiat:USD through the layers /v1/price uses, so the derived price rests on exactly what the USD
// endpoint serves. It does NOT re-enter [Server.priceFallback] (recursion into [tryUSDAnchoredFiatCross]); a withheld leg is
// neither served nor reported absent. It reads no freeze marker ([Server.resolveFrozenServeFor]).
func (s *Server) resolveUSDLeg(
	ctx context.Context, asset canonical.Asset,
) (snap PriceSnapshot, sources []string, served canonical.Asset, stale, ok, withheld bool) {
	usd := defaultPriceQuote
	snap, sources, stale, served, err := s.readPriceWithAliasesServed(ctx, s.Prices, asset, usd)
	switch {
	case errors.Is(err, ErrPriceWithheld):
		return PriceSnapshot{}, nil, canonical.Asset{}, false, false, true
	case err == nil:
		// A fallback is served without the handler's normalization, so the raw bucket scales here.
		base := served
		if base.IsZero() {
			base = asset
		}
		s.normalizeRawPriceSnapshot(&snap, base, usd)
		return snap, sources, served, stale, true, false
	case !errors.Is(err, ErrPriceNotFound):
		return PriceSnapshot{}, nil, canonical.Asset{}, false, false, false
	}
	// A fallback USD leg is below the closed-bucket contract: stale.
	if snap, srcs, _, _, ok := s.tryRedisVWAPFallback(ctx, asset, usd); ok {
		return snap, srcs, canonical.Asset{}, true, true, false
	}
	snap, srcs, ok, proxyWithheld := s.tryStablecoinFiatProxy(ctx, asset, usd)
	if ok {
		return snap, srcs, canonical.Asset{}, true, true, false
	}
	return PriceSnapshot{}, nil, canonical.Asset{}, false, false, proxyWithheld != ""
}

// primaryFXSource is the forex worker's primary feed, credited for an entry with no Source.
const primaryFXSource = "massive"

// fxSourceOf names the feed that published c's rate, so a price converted through the standby
// credits the standby, not the primary.
func fxSourceOf(c CurrencyEntry) string {
	if c.Source == "" {
		return primaryFXSource
	}
	return c.Source
}

// appendFXSource adds the FX feed to the USD leg's sources without dropping them: an audit of a
// converted price needs both the venues that set the USD price AND the feed that converted it.
func appendFXSource(sources []string, fxSource string) []string {
	for _, s := range sources {
		if s == fxSource {
			return sources
		}
	}
	out := make([]string, 0, len(sources)+1)
	out = append(out, sources...)
	out = append(out, fxSource)
	sort.Strings(out)
	return out
}

// proxyPairGate is OPTIONALLY implemented by the [PriceReader] so [Server.tryStablecoinFiatProxy]
// can skip empty proxy pairs with a bounded recent-existence probe over prices_1m.
type proxyPairGate interface {
	// RecentClosedVWAP1mExists reports whether base/quote has a closed 1-minute VWAP bucket (either
	// stored direction) inside the price surface's freshness horizon. False means no live proxy
	// pair: skip the peg. An error means unknown: let the caller try LatestPrice anyway.
	RecentClosedVWAP1mExists(ctx context.Context, base, quote canonical.Asset) (bool, error)
}

// tryStablecoinFiatProxy rewrites X/fiat:USD to X/<classic USD peg> at read time (no on-chain trade quotes in fiat:USD, so the
// literal pair has no prices_1m rows). A non-peg walks usd_pegged_classic_assets ([Server.walkUSDPegs]); a declared peg is priced
// via its XLM book ([Server.crossDeclaredPegThroughXLM]) and the declaration answers only when the cross has nothing (a depeg is
// when they disagree), never via another declared peg. Snapshots are triangulated; sticky `withheld` (ErrPriceWithheld) must not
// collapse into not-found, and a refused declared-peg cross is withheld (`upstream_leg`), not replaced by a flat 1.0.
func (s *Server) tryStablecoinFiatProxy(ctx context.Context, asset, quote canonical.Asset) (PriceSnapshot, []string, bool, PriceWithheldReason) {
	// Self-peg: a `crypto:<STABLE>` ticker priced in the fiat it tracks (aggregate.FiatProxy); a
	// depeg surfaces via divergence, not here. Runs only after the literal read missed, so an
	// observed bucket still wins over the declaration.
	if proxy, isStable := aggregate.FiatProxy(asset); isStable && proxy.Equal(quote) {
		return s.declaredPegSnapshot(asset, quote), nil, true, ""
	}
	// Returns before the walk carry no withheld reason: no peg leg has been read yet.
	if quote.Type != canonical.AssetFiat || quote.Code != "USD" {
		return PriceSnapshot{}, nil, false, ""
	}
	if len(s.USDPeggedClassics) == 0 || s.Prices == nil {
		return PriceSnapshot{}, nil, false, ""
	}
	if s.isDeclaredUSDPeg(asset) {
		snap, srcs, verdict, withheld := s.crossDeclaredPegThroughXLM(ctx, asset, quote)
		switch verdict {
		case pegXLMLegPriced:
			return snap, srcs, true, ""
		case pegXLMLegRefused:
			// The peg's own XLM book names its reason (`substance` below the floor, as the listing
			// says); a withheld XLM/USD pivot lies beyond that market and reads `upstream_leg`.
			if withheld == "" {
				withheld = PriceWithheldUnattributed
			}
			return PriceSnapshot{}, nil, false, withheld
		case pegXLMLegNoMarket, pegXLMLegReadFailed, pegXLMLegFlagged:
		}
		// No market priced this peg and none was refused: publish the declaration with nil sources
		// (empty is not "single-sourced").
		return s.declaredPegSnapshot(asset, quote), nil, true, ""
	}
	snap, srcs, ok, withheld := s.walkUSDPegs(ctx, asset, quote)
	if ok {
		return snap, srcs, true, withheld
	}
	return PriceSnapshot{}, nil, false, withheld
}

// isDeclaredUSDPeg reports whether asset is a usd_pegged_classic_assets entry under ANY spelling ([sameAsset] folds the SAC into the
// classic id), else the SAC would take the sibling walk.
func (s *Server) isDeclaredUSDPeg(asset canonical.Asset) bool {
	for _, peg := range s.USDPeggedClassics {
		if sameAsset(peg, asset) {
			return true
		}
	}
	return false
}

// walkUSDPegs is [Server.tryStablecoinFiatProxy]'s allow-list walk: the first observed asset/<peg> price in priority order, re-quoted to
// fiat:USD; a declared peg never reaches it.
func (s *Server) walkUSDPegs(
	ctx context.Context, asset, quote canonical.Asset,
) (snapshot PriceSnapshot, sources []string, ok bool, withheld PriceWithheldReason) {
	// Empty-proxy-pair gate. On a VWAP miss LatestPrice falls through to an UNBOUNDED last-trade
	// scan, which a Soroban token that only trades vs XLM would pay once PER PEG. A probe ERROR falls
	// through to LatestPrice so a blip never suppresses a real price.
	gate, _ := s.Prices.(proxyPairGate)
	for _, peg := range s.USDPeggedClassics {
		if gate != nil {
			exists, gerr := gate.RecentClosedVWAP1mExists(ctx, asset, peg)
			if gerr == nil && !exists {
				continue
			}
		}
		snap, srcs, _, err := s.Prices.LatestPrice(ctx, asset, peg)
		if err != nil {
			// A WITHHELD verdict is not a miss. Walk on, a later peg may serve; if none does, the
			// caller reports the loudest reason ([walkWithheldReason]) rather than not-found.
			if errors.Is(err, ErrPriceWithheld) {
				withheld = walkWithheldReason(withheld, err)
			}
			continue
		}
		// LatestPrice returns the RAW asset/<peg> ratio; this is the SINGLE place the proxy tier
		// is decimals-corrected, against the ACTUAL legs (asset, peg), NOT the requested fiat:USD.
		s.normalizeRawPriceSnapshot(&snap, asset, peg)
		// The wire response reflects what the user asked for, not the proxy peg.
		snap.Quote = quote.String()
		return snap, srcs, true, withheld
	}
	return PriceSnapshot{}, nil, false, withheld
}

// walkWithheldRank orders the reasons [Server.walkUSDPegs] reports when no peg serves: the asset's
// own market below the floor outranks a withheld peg leg, which outranks a refusal that stated
// no reason.
var walkWithheldRank = map[PriceWithheldReason]int{
	PriceWithheldUnattributed: 1,
	PriceWithheldUpstreamLeg:  2,
	PriceWithheldSubstance:    3,
}

// walkWithheldReason folds one refused peg read into the walk's running reason. The peg pair is
// the asset's own market, so `substance` (as the listing says) and an unstated reason pass
// through; any other cause (a flagged peg issuer, a refused cross) lies beyond it and is
// `upstream_leg`.
func walkWithheldReason(have PriceWithheldReason, err error) PriceWithheldReason {
	got := priceWithheldReason(err)
	if got != PriceWithheldSubstance && got != PriceWithheldUnattributed {
		got = PriceWithheldUpstreamLeg
	}
	if walkWithheldRank[got] > walkWithheldRank[have] {
		return got
	}
	return have
}

// crossDeclaredPegThroughXLM prices a declared USD peg via its XLM book, price(peg/XLM) x price(XLM/fiat:USD) in exact big.Rat
// (ADR-0003, [crossThroughPivot]); a non-positive or missing leg is a miss. The pivot is read direct, never via the Redis
// composite or stablecoin proxy (it would cancel to 1). Each native/SAC-quoted miss is an unbounded last-trade scan, so it sits
// behind a [proxyPairGate] probe. A withheld leg stops the spelling walk: the scam gate can't fire on a SAC base, so the SAC book
// would publish the declined market. observed_at is the older leg's. Beside pegXLMLegRefused travels the reason: the peg book's
// own, or `upstream_leg` when the pivot was refused.
func (s *Server) crossDeclaredPegThroughXLM(
	ctx context.Context, asset, quote canonical.Asset,
) (PriceSnapshot, []string, pegXLMLegVerdict, PriceWithheldReason) {
	xlm := canonical.NativeAsset()
	if sameAsset(asset, xlm) {
		return PriceSnapshot{}, nil, pegXLMLegNoMarket, ""
	}
	pegLeg, pegSources, verdict, withheld := s.readDeclaredPegXLMLeg(ctx, asset)
	if verdict != pegXLMLegPriced {
		return PriceSnapshot{}, nil, verdict, withheld
	}
	xlmLeg, xlmSources, _, err := s.readPriceWithAliases(ctx, s.Prices, xlm, quote)
	switch {
	case errors.Is(err, ErrPriceWithheld):
		// XLM/USD is the pivot's market, not the peg's own: whatever its reason, it is upstream.
		return PriceSnapshot{}, nil, pegXLMLegRefused, PriceWithheldUpstreamLeg
	case errors.Is(err, ErrPriceNotFound):
		return PriceSnapshot{}, nil, pegXLMLegNoMarket, ""
	case err != nil:
		return PriceSnapshot{}, nil, pegXLMLegReadFailed, ""
	}
	// Both legs are RAW ratios; decimals-correct each before taking the product.
	s.normalizeRawPriceSnapshot(&xlmLeg, xlm, quote)
	price, ok := crossThroughPivot(pegLeg.Price, xlmLeg.Price)
	if !ok {
		return PriceSnapshot{}, nil, pegXLMLegNoMarket, ""
	}
	observedAt := pegLeg.ObservedAt
	if xlmLeg.ObservedAt.Time().Before(observedAt.Time()) {
		observedAt = xlmLeg.ObservedAt
	}
	// The wider window governs; a last-trade-tail leg carries none.
	windowSeconds := pegLeg.WindowSeconds
	if xlmLeg.WindowSeconds > windowSeconds {
		windowSeconds = xlmLeg.WindowSeconds
	}
	return PriceSnapshot{
		AssetID:       asset.String(),
		Quote:         quote.String(),
		Price:         price,
		PriceType:     "vwap",
		ObservedAt:    observedAt,
		WindowSeconds: windowSeconds,
	}, unionSources(pegSources, xlmSources), pegXLMLegPriced, ""
}

// readDeclaredPegXLMLeg reads asset/XLM for [Server.crossDeclaredPegThroughXLM]: each peg spelling against each XLM alias; the walk
// advances only on pegXLMLegNoMarket.
// pegXLMLegPriced ends it (fresh beats stale only within a spelling; a zero/unparsable price counts as answered so one degenerate print
// can't hand the deep book to a thin pool).
// pegXLMLegRefused/Flagged end it (the scam gate never fires on a non-classic base); pegXLMLegReadFailed ends it and the declaration
// answers. Canonical-first order (SAC last).
func (s *Server) readDeclaredPegXLMLeg(
	ctx context.Context, asset canonical.Asset,
) (PriceSnapshot, []string, pegXLMLegVerdict, PriceWithheldReason) {
	xlmForms := assetAliases(canonical.NativeAsset())
	for _, base := range assetAliases(canonical.CanonicalAsset(asset)) {
		snap, srcs, verdict, withheld := s.readPegXLMLegForSpelling(ctx, base, xlmForms)
		if verdict != pegXLMLegNoMarket {
			// Only "found nothing" reaches the next spelling.
			return snap, srcs, verdict, withheld
		}
	}
	return PriceSnapshot{}, nil, pegXLMLegNoMarket, ""
}

// pegXLMLegVerdict is what one read on the declared peg's XLM leg SAID: collapsing to ok=false would turn a refusal or broken
// read into "no market, try the next spelling" and ultimately into the declaration. Values ascend by how much a read says, so a
// spelling's verdict is the maximum of its combinations'.
type pegXLMLegVerdict int

const (
	// pegXLMLegNoMarket: the probe saw no recent closed bucket, or the read was ErrPriceNotFound.
	// The ONLY verdict that lets the walk move to the next spelling.
	pegXLMLegNoMarket pegXLMLegVerdict = iota
	// pegXLMLegReadFailed: an error that is neither not-found nor withheld (transport, timeout).
	// Says nothing about whether a market exists, so the walk stops.
	pegXLMLegReadFailed
	// pegXLMLegFlagged: ErrPriceWithheld for a directory-flagged issuer. The walk stops and the declaration still answers; the
	// scam gate keys on the classic issuer, so its verdict stops a SAC spelling republishing that issuer's refused market.
	pegXLMLegFlagged
	// pegXLMLegRefused: any other ErrPriceWithheld (substance gate on the alias union, or no stated reason); the read's own
	// [PriceWithheldReason] travels beside it. The walk stops and the route withholds rather than print
	// the declaration over it; ranked above pegXLMLegFlagged so a mix fails closed.
	pegXLMLegRefused
	// pegXLMLegPriced: a snapshot was read; a stale snapshot is still priced.
	pegXLMLegPriced
)

// readPegXLMLegForSpelling reads ONE peg spelling against every XLM form; pegXLMLegNoMarket (all forms gate-missed or not-found) is the
// only condition to try the next spelling.
// Among answering forms fresh beats stale (alias order alone would take an old `native` over a fresh SAC book: disjoint venue
// populations), confined to one spelling.
// A refusal or read failure on one XLM form does NOT stop this spelling; only when none priced does it decide. Among refusals a
// stated reason outranks an unattributed one.
func (s *Server) readPegXLMLegForSpelling(
	ctx context.Context, base canonical.Asset, xlmForms []canonical.Asset,
) (PriceSnapshot, []string, pegXLMLegVerdict, PriceWithheldReason) {
	var staleSnap PriceSnapshot
	var staleSources []string
	staleFound := false
	spelling := pegXLMLegNoMarket
	var refused PriceWithheldReason
	for _, xlm := range xlmForms {
		snap, srcs, stale, verdict, reason := s.readPegXLMLegPair(ctx, base, xlm)
		if verdict != pegXLMLegPriced {
			// Keep the loudest verdict: a refusal or failure outranks not-found.
			spelling = max(spelling, verdict)
			if verdict == pegXLMLegRefused && (refused == "" || refused == PriceWithheldUnattributed) {
				refused = reason
			}
			continue
		}
		if !stale {
			return snap, srcs, pegXLMLegPriced, ""
		}
		if !staleFound {
			staleSnap, staleSources, staleFound = snap, srcs, true
		}
	}
	if staleFound {
		return staleSnap, staleSources, pegXLMLegPriced, ""
	}
	return PriceSnapshot{}, nil, spelling, refused
}

// readPegXLMLegPair is one (peg spelling, XLM form) combination of the [Server.readDeclaredPegXLMLeg] walk; it must keep the
// [pegXLMLegVerdict] distinct (a flagged issuer, [PriceWithheldScamIssuer], is pegXLMLegFlagged; other ErrPriceWithheld is
// pegXLMLegRefused, carrying the read's own reason). A probe ERROR falls through to the read so a blip never hides a price.
func (s *Server) readPegXLMLegPair(
	ctx context.Context, base, xlm canonical.Asset,
) (PriceSnapshot, []string, bool, pegXLMLegVerdict, PriceWithheldReason) {
	if gate, wired := s.Prices.(proxyPairGate); wired {
		if exists, gerr := gate.RecentClosedVWAP1mExists(ctx, base, xlm); gerr == nil && !exists {
			return PriceSnapshot{}, nil, false, pegXLMLegNoMarket, ""
		}
	}
	snap, srcs, stale, err := s.Prices.LatestPrice(ctx, base, xlm)
	switch {
	case errors.Is(err, ErrPriceWithheld) && priceWithheldReason(err) == PriceWithheldScamIssuer:
		return PriceSnapshot{}, nil, false, pegXLMLegFlagged, ""
	case errors.Is(err, ErrPriceWithheld):
		return PriceSnapshot{}, nil, false, pegXLMLegRefused, priceWithheldReason(err)
	case errors.Is(err, ErrPriceNotFound):
		return PriceSnapshot{}, nil, false, pegXLMLegNoMarket, ""
	case err != nil:
		return PriceSnapshot{}, nil, false, pegXLMLegReadFailed, ""
	}
	s.normalizeRawPriceSnapshot(&snap, base, xlm)
	return snap, srcs, stale, pegXLMLegPriced, ""
}

// unionSources merges two legs' source sets (deduplicated, sorted): an audit of a crossed price
// needs every venue behind it.
func unionSources(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, src := range list {
			if _, dup := seen[src]; dup {
				continue
			}
			seen[src] = struct{}{}
			out = append(out, src)
		}
	}
	sort.Strings(out)
	return out
}

// declaredPegPrice is the operator's standing 1:1 declaration at 12 fractional digits; the bytes
// are part of the wire contract.
const declaredPegPrice = "1.000000000000"

// declaredPegSnapshot renders the peg declaration as a snapshot: served for a declared peg with NO market observation, and for a
// `crypto:<STABLE>` ticker in its own fiat.
// ObservedAt is the adoption time ([Server.pegDeclaredAt]), never time.Now(): a clock stamp would pass for an observation. price_type
// "peg" is the durable signal.
func (s *Server) declaredPegSnapshot(asset, quote canonical.Asset) PriceSnapshot {
	return PriceSnapshot{
		AssetID:    asset.String(),
		Quote:      quote.String(),
		Price:      declaredPegPrice,
		PriceType:  "peg",
		ObservedAt: WireTime(s.pegDeclaredAt),
	}
}

// tryFiatCrossRate synthesises a fiat-vs-fiat price from the CurrenciesReader's USD-base snapshot, the last resort (rate_usd[X] is "1 USD
// = N X", so 1 X in Y = rate_usd[Y] / rate_usd[X]).
// PriceType is "vwap" (the forex feed is volume-weighted); ok=false when a side isn't fiat, the reader is cold, or a rate is missing or
// zero.
func (s *Server) tryFiatCrossRate(asset, quote canonical.Asset) (PriceSnapshot, []string, bool) {
	if asset.Type != canonical.AssetFiat || quote.Type != canonical.AssetFiat {
		return PriceSnapshot{}, nil, false
	}
	if s.Currencies == nil {
		return PriceSnapshot{}, nil, false
	}
	snap := s.Currencies.Latest()
	if snap == nil {
		return PriceSnapshot{}, nil, false
	}

	// Linear scan: the slice is small (~110) and this is a last-resort fallback.
	var rateAsset, rateQuote float64
	var foundAsset, foundQuote bool
	var sources []string
	// observedAt is pulled BACK to the older leg's UpdatedAt: the forex worker may be HOLDING a
	// leg's last guarded rate, and the publication time would claim freshness it lacks.
	observedAt := snap.PublishedAt
	for _, c := range snap.Currencies {
		if c.Ticker == asset.Code {
			rateAsset = c.RateUSD
			foundAsset = true
			observedAt = olderNonZero(observedAt, c.UpdatedAt)
			sources = appendFXSource(sources, fxSourceOf(c))
		}
		if c.Ticker == quote.Code {
			rateQuote = c.RateUSD
			foundQuote = true
			observedAt = olderNonZero(observedAt, c.UpdatedAt)
			sources = appendFXSource(sources, fxSourceOf(c))
		}
		if foundAsset && foundQuote {
			break
		}
	}
	if !foundAsset || rateAsset <= 0 {
		return PriceSnapshot{}, nil, false
	}
	// rate_usd[USD] is implicitly 1; the snapshot needn't carry a USD entry.
	if !foundQuote {
		if quote.Code != "USD" {
			return PriceSnapshot{}, nil, false
		}
		rateQuote = 1
	}
	// [Cache.Latest] never expires, so without this bound a forex outage would serve the last fetch
	// forever. observedAt is the older leg's, so one check covers a dead worker and a held ticker.
	if fxCrossStale(observedAt, s.fxCrossMaxAge) {
		return PriceSnapshot{}, nil, false
	}
	// Exact big.Rat, not float64 (ADR-0003); each float64 rate goes via its shortest round-trip decimal.
	rateQuoteRat, okQ := new(big.Rat).SetString(strconv.FormatFloat(rateQuote, 'f', -1, 64))
	rateAssetRat, okA := new(big.Rat).SetString(strconv.FormatFloat(rateAsset, 'f', -1, 64))
	if !okQ || !okA || rateAssetRat.Sign() == 0 {
		return PriceSnapshot{}, nil, false
	}
	priceStr := formatCrossRate(new(big.Rat).Quo(rateQuoteRat, rateAssetRat))
	return PriceSnapshot{
		AssetID:    asset.String(),
		Quote:      quote.String(),
		Price:      priceStr,
		PriceType:  "vwap",
		ObservedAt: WireTime(observedAt),
	}, sources, true
}

// olderNonZero returns the earlier of a and b, ignoring a zero b.
func olderNonZero(a, b time.Time) time.Time {
	if !b.IsZero() && b.Before(a) {
		return b
	}
	return a
}

// formatCrossRate serialises a fiat cross-rate to a decimal string with up to 15 places, trailing zeros trimmed (bounded for non-
// terminating ratios like 1/3);
// a rate too small for 15 places gets more ([priceRenderScale]) so a positive cross-rate never serves as "0".
func formatCrossRate(r *big.Rat) string {
	s := r.FloatString(priceRenderScale(r, 15))
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

// heldWindow is the window a per-pair enrichment lookup must ask for: a held
// (frozen) value was priced by its own window, not the default one.
func heldWindow(frozen bool, snap PriceSnapshot, def time.Duration) time.Duration {
	if frozen && snap.WindowSeconds > 0 {
		return time.Duration(snap.WindowSeconds) * time.Second
	}
	return def
}

// attachConfidence populates snap.Confidence and its factors from the wired ConfidenceLooker.
// Best-effort: misses and read errors leave them nil.
func (s *Server) attachConfidence(r *http.Request, snap *PriceSnapshot, asset, quote canonical.Asset, window time.Duration) {
	if s.Confidence == nil {
		return
	}
	// Loop the alias set as the price read does: a key built from the raw request asset (`native`)
	// finds nothing, and clients read absent confidence as "unknown".
	for _, a := range assetAliases(asset) {
		got, ok, err := s.Confidence.LookupConfidence(r.Context(), a, quote, window)
		if err != nil {
			if !clientAborted(r, err) {
				s.logger.Warn("confidence lookup failed",
					"err", err, "asset", a.String(), "quote", quote.String())
			}
			return
		}
		if !ok {
			continue // cache miss on this spelling — try the next alias
		}
		c := got.Confidence
		snap.Confidence = &c
		f := got.Factors
		snap.ConfidenceFactors = &f
		return
	}
}

// lookupDivergenceFlag is the best-effort divergence lookup for every surface serving a value the verdict speaks to; (false,
// false) when unwired, no verdict, a different window or on error (a price without a verdict beats a 5xx on a Redis blip). Only
// the SERVED spelling is asked: walking aliases would attach crypto:XLM's CEX-checked verdict to native's SDEX price. window is
// the served window, or 0 for the current price. A context-caused error is not logged; the test is on the ERROR, not ctx.Err(),
// so a real store failure still is.
func (s *Server) lookupDivergenceFlag(ctx context.Context, asset, quote canonical.Asset, window time.Duration) (firing, checked bool) {
	if s.Divergence == nil {
		return false, false
	}
	firing, checked, verdictWindow, err := s.Divergence.DivergenceFiringFor(ctx, asset, quote)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.logger.Warn("divergence lookup failed",
				"err", err, "asset", asset.String(), "quote", quote.String())
		}
		return false, false
	}
	if window > 0 && verdictWindow != window {
		return false, false
	}
	return firing, checked
}

// attachCompositeFlags surfaces the router-quality flags (diverged, rerouted) from [cachekeys.VWAPCompositeMeta] via the OPTIONAL
// [CompositeMetaLooker]; any miss leaves them unset.
// requireTriangulated: true on the ?window= surface (a direct writer may own its per-(pair,window) key); false on the headline surface,
// which reads prices_1m first.
func (s *Server) attachCompositeFlags(r *http.Request, flags *Flags, asset, quote canonical.Asset, window time.Duration, requireTriangulated bool) {
	if requireTriangulated && !flags.Triangulated {
		return
	}
	looker, ok := s.Triangulated.(CompositeMetaLooker)
	if !ok {
		return
	}
	raw, found, err := looker.LookupCompositeMeta(r.Context(), asset, quote, window)
	if err != nil {
		if !clientAborted(r, err) {
			s.logger.Warn("composite meta lookup failed",
				"err", err, "asset", asset.String(), "quote", quote.String())
		}
		return
	}
	if !found {
		return // no router meta for this composite — leave flags unset
	}
	meta, err := cachekeys.DecodeCompositeMeta(raw)
	if err != nil {
		// Enrichment noise, not a serving failure: the price is already computed.
		s.logger.Warn("composite meta decode failed",
			"err", err, "asset", asset.String(), "quote", quote.String())
		return
	}
	flags.Diverged = meta.Diverged
	flags.Rerouted = meta.Rerouted
	flags.PivotUnverified = meta.PivotUnverified
}

// lookupFrozen asks the FrozenLooker (when wired) whether the latest bucket was frozen; checked=false means no verdict (unwired or marker
// read failed), so "unknown" differs from "confirmed not frozen".
func (s *Server) lookupFrozen(r *http.Request, asset, quote canonical.Asset) (frozen, checked bool) {
	if s.Freeze == nil {
		return false, false
	}
	frozen, err := s.Freeze.FrozenForPair(r.Context(), asset, quote)
	if err != nil {
		if !clientAborted(r, err) {
			obs.APIFreezeLookupFailuresTotal.Inc()
			s.logger.Warn("freeze lookup failed",
				"err", err,
				"asset", asset.String(),
				"quote", quote.String())
		}
		return false, false
	}
	return frozen, true
}

// frozenServe is the outcome of [Server.resolveFrozenServe]. "Frozen but nothing to serve" is its own state: read as "not frozen"
// it publishes the bucket the freeze withholds; read as "held" it publishes an empty price.
type frozenServe int

const (
	// frozenServeNotFrozen: no live freeze; serve what was read.
	frozenServeNotFrozen frozenServe = iota
	// frozenServeHeld: frozen, and resolution.snapshot is the held value.
	frozenServeHeld
	// frozenServeNothingHeld: frozen, no held value readable; the caller serves NOTHING.
	frozenServeNothingHeld
)

// frozenResolution carries [Server.resolveFrozenServe]'s verdict; snapshot, sources and triangulated are meaningful only for
// frozenServeHeld.
// checked=false means the marker was unreadable: outcome is frozenServeNotFrozen but means "unknown", so derive no other facts (e.g.
// single_source) from it.
type frozenResolution struct {
	outcome      frozenServe
	checked      bool
	snapshot     PriceSnapshot
	sources      []string
	triangulated bool
	// pairBase is the spelling whose marker fired; composite-meta must ask for the same pair.
	pairBase canonical.Asset
}

// frozenHeldWindows are the aggregator windows a freeze can hold a value for, smallest first: the marker is per pair (ADR-0019) and can't
// say which, so smallest stays closest to the 1m bucket.
var frozenHeldWindows = []time.Duration{5 * time.Minute, time.Hour, 24 * time.Hour}

// resolveFrozenServe makes `flags.frozen` true honestly by making the VALUE the one the freeze holds (ADR-0019), from the aggregator's
// VWAP cache (keepFrozenVWAPAlive):
// prices_1m is never gated by the anomaly checker, so its newest bucket on a frozen pair is the very bucket the freeze refused.
// `served` is the alias the closed read came from (zero on a miss); a `native` request answered from crypto:XLM's bucket is governed by
// crypto:XLM's freeze only ([Server.frozenPairBase]).
func (s *Server) resolveFrozenServe(r *http.Request, requested, served, quote canonical.Asset) frozenResolution {
	pairBase, frozen, checked := s.frozenPairBase(r, requested, served, quote)
	if !frozen {
		return frozenResolution{outcome: frozenServeNotFrozen, checked: checked}
	}
	if s.Triangulated == nil {
		return frozenResolution{outcome: frozenServeNothingHeld, checked: checked}
	}
	for _, window := range frozenHeldWindows {
		v, found, err := s.lookupCachedVWAP(r.Context(), pairBase, quote, window)
		if err != nil {
			if !clientAborted(r, err) {
				s.logger.Warn("frozen pair: held-value lookup failed",
					"err", err, "asset", pairBase.String(), "quote", quote.String(), "window", window)
			}
			continue
		}
		if !found {
			continue
		}
		return frozenResolution{
			outcome:      frozenServeHeld,
			checked:      true,
			sources:      []string{},
			triangulated: v.Triangulated,
			pairBase:     pairBase,
			snapshot: withCoverage(PriceSnapshot{
				AssetID:   requested.String(),
				Quote:     quote.String(),
				Price:     v.Value,
				PriceType: "vwap",
				// When the held value was observed, not read: a hold makes them differ.
				ObservedAt:    WireTime(v.ObservedAt),
				WindowSeconds: int(window / time.Second),
			}, v.Coverage),
		}
	}
	return frozenResolution{outcome: frozenServeNothingHeld, checked: checked}
}

// resolveFrozenServeFor is [Server.resolveFrozenServe] for the snapshot in hand. An ADR-0051 cross is governed by its USD leg's marker
// (as /v1/price?quote=fiat:USD reads it);
// a frozen leg serves its held value converted at a fixing, or nothing, never the refused bucket times the rate.
func (s *Server) resolveFrozenServeFor(r *http.Request, snap PriceSnapshot, requested, served, quote canonical.Asset) frozenResolution {
	if c := snap.cryptoCross; c != nil {
		return s.resolveFrozenCryptoCross(r, c, requested, quote)
	}
	leg := snap.usdLeg
	if leg == nil {
		return s.resolveFrozenServe(r, requested, served, quote)
	}
	held := s.resolveFrozenServe(r, requested, leg.served, defaultPriceQuote)
	if held.outcome != frozenServeHeld {
		return held
	}
	// Converting at the fixing bound to its own bucket end keeps the hold one answer.
	bound, err := s.bindFXFixings(r.Context(), []string{quote.Code}, time.Time(held.snapshot.ObservedAt))
	if err != nil {
		return frozenResolution{outcome: frozenServeNothingHeld, checked: held.checked}
	}
	b := bound[quote.Code]
	converted, ok := convertAtFixing(held.snapshot, held.sources, quote, b)
	if !ok {
		return frozenResolution{outcome: frozenServeNothingHeld, checked: held.checked}
	}
	held.snapshot = converted
	held.sources, held.triangulated = []string{b.Source}, true
	return held
}

// frozenPairBase reports which spelling's freeze marker GOVERNS this response. With a closed bucket read, only its alias's marker
// counts (the requested literal judges a DIFFERENT venue population); with none (`served` zero) every spelling governs, since the
// fallback chain doesn't report its leg ([Server.frozenAnyAlias]).
func (s *Server) frozenPairBase(r *http.Request, requested, served, quote canonical.Asset) (asset canonical.Asset, frozen, checked bool) {
	if served.IsZero() {
		return s.frozenAnyAlias(r, requested, quote)
	}
	frozen, checked = s.lookupFrozen(r, served, quote)
	if !checked {
		return canonical.Asset{}, false, false
	}
	if frozen {
		return served, true, true
	}
	return canonical.Asset{}, false, true
}

// frozenAnyAlias reads the freeze marker of every spelling of requested against quote; a frozen one governs at once, else checked
// is true only when every read succeeded (one failed read is "unknown", never "confirmed not frozen").
func (s *Server) frozenAnyAlias(r *http.Request, requested, quote canonical.Asset) (asset canonical.Asset, frozen, checked bool) {
	allChecked := true
	for _, alias := range assetAliases(requested) {
		aliasFrozen, aliasChecked := s.lookupFrozen(r, alias, quote)
		if !aliasChecked {
			allChecked = false
			continue
		}
		if aliasFrozen {
			return alias, true, true
		}
	}
	return canonical.Asset{}, false, allChecked
}

// writeFrozenNothingHeldProblem is the single-asset refusal for frozenServeNothingHeld: a 503, not a 404 (prices exist and the
// freeze clears on its own, so retry is right), with its own title and detail since the "market too thin" wording would misstate
// why.
func writeFrozenNothingHeldProblem(w http.ResponseWriter, r *http.Request, asset, quote canonical.Asset) {
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/price-unavailable",
		"Price frozen — no last-known-good value available", http.StatusServiceUnavailable,
		asset.String()+" / "+quote.String()+
			" is frozen by anomaly detection (ADR-0019) and no last-known-good value is currently held,"+
			" so the refused bucket is not published in its place — retry shortly,"+
			" or read the raw market via /v1/price/tip or /v1/observations and apply your own judgement")
}

// holdFrozenBatchRow is [Server.resolveFrozenServe] for one /v1/price/batch row: a frozen row carries the held value; one with
// nothing held is omitted, the batch contract's only spelling of "no price" (callers probe /v1/price for the reason).
func (s *Server) holdFrozenBatchRow(r *http.Request, row batchRowResult, served, quote canonical.Asset) batchRowResult {
	held := s.resolveFrozenServeFor(r, row.snap, row.asset, served, quote)
	switch held.outcome {
	case frozenServeNotFrozen:
		return row
	case frozenServeNothingHeld:
		return batchRowResult{skip: true}
	case frozenServeHeld:
	}
	row.snap, row.sources, row.triangulated = held.snapshot, held.sources, held.triangulated
	row.stale, row.frozen = true, true
	return row
}

// handlePriceBatch serves GET /v1/price/batch?asset_ids=A,B,C&quote=<id>: missing observations are omitted, not 404'd, so callers see
// which assets are missing.
// asset_ids count 1..100 (priceBatchMaxAssets; POST up to 1000), de-duplicated server-side; the envelope Stale flag is the OR of per-row
// flags, as on /v1/price.
func (s *Server) handlePriceBatch(w http.ResponseWriter, r *http.Request) {
	// `pairs=` is an alias for `asset_ids=` (CG-style clients call it that). Passing both is a
	// 400 rather than a silent precedence.
	rawIDs := r.URL.Query().Get("asset_ids")
	rawPairs := r.URL.Query().Get("pairs")
	if rawIDs != "" && rawPairs != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-parameter",
			"`asset_ids` and `pairs` are mutually exclusive", http.StatusBadRequest,
			"both query parameters refer to the same value — pick one (this endpoint's canonical form is `asset_ids=`; `pairs=` is accepted as an alias for cross-endpoint compatibility)")
		return
	}
	if rawIDs == "" {
		rawIDs = rawPairs
	}
	if rawIDs == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-asset-ids",
			"Missing asset_ids parameter", http.StatusBadRequest,
			"asset_ids query parameter is required (comma-separated; `pairs=` is accepted as an alias)")
		return
	}
	s.runPriceBatch(w, r, strings.Split(rawIDs, ","), r.URL.Query().Get("quote"), priceBatchMaxAssets)
}

// handlePriceBatchPost serves POST /v1/price/batch with JSON body {"asset_ids": [...], "quote": "..."}: GET semantics with the
// ceiling raised to 1000 (a 1000-entry query string exceeds most reverse-proxies' default 8 KiB header limit).
func (s *Server) handlePriceBatchPost(w http.ResponseWriter, r *http.Request) {
	// 1 MiB is plenty for 1000 asset ids (well under 100 KiB) and bounds parse memory.
	const maxBody = 1 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	var body struct {
		AssetIDs []string `json:"asset_ids"`
		Quote    string   `json:"quote"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-body",
			"Invalid JSON body", http.StatusBadRequest, err.Error())
		return
	}
	if len(body.AssetIDs) == 0 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-asset-ids",
			"Missing asset_ids", http.StatusBadRequest,
			"request body must include a non-empty asset_ids array")
		return
	}
	s.runPriceBatch(w, r, body.AssetIDs, body.Quote, priceBatchMaxAssetsPOST)
}

// runPriceBatch is the shared core of GET + POST /v1/price/batch: trims, de-duplicates (first-occurrence order), enforces `limit`, parses
// the quote (default fiat:USD).
// Each helper writes its own problem+json on failure and signals via a sentinel return.
func (s *Server) runPriceBatch(w http.ResponseWriter, r *http.Request, rawIDs []string, rawQuote string, limit int) {
	if s.Prices == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Price serving not configured", http.StatusServiceUnavailable,
			"this deployment has no PriceReader wired — check binary configuration")
		return
	}
	ids, ok := s.parsePriceBatchIDs(w, r, rawIDs, limit)
	if !ok {
		return
	}
	quote, ok := s.parsePriceBatchQuote(w, r, rawQuote)
	if !ok {
		return
	}
	// Charge one rate-limit token PER ID before resolving any: the pre-dispatch charge is one token
	// per request, so a 1000-id POST would otherwise buy a thousand resolutions for one. Charged on
	// the de-duplicated count (the work done) and only once well-formed, so a 400 costs the base token.
	if !middleware.ChargeRateLimit(w, r, len(ids)) {
		return
	}
	// The monthly meter counts the same ids, one unit per price; after the rate-limit charge so a
	// 429 meters one unit.
	middleware.ChargeUsage(r, len(ids))
	s.lookupPriceBatch(w, r, ids, quote)
}

// parsePriceBatchIDs trims, de-duplicates and length-checks asset_ids; ok=false means a problem
// was written.
func (s *Server) parsePriceBatchIDs(w http.ResponseWriter, r *http.Request, rawIDs []string, limit int) ([]string, bool) {
	ids := make([]string, 0, len(rawIDs))
	seen := make(map[string]struct{}, len(rawIDs))
	for _, p := range rawIDs {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		ids = append(ids, t)
	}
	if len(ids) == 0 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-asset-ids",
			"Missing asset_ids", http.StatusBadRequest,
			"asset_ids must contain at least one non-empty id")
		return nil, false
	}
	if len(ids) > limit {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/too-many-assets",
			"Too many assets", http.StatusBadRequest,
			fmt.Sprintf("asset_ids may contain at most %d entries", limit))
		return nil, false
	}
	return ids, true
}

// parsePriceBatchQuote parses the optional quote (default fiat:USD); ok=false means a 400 was written.
func (s *Server) parsePriceBatchQuote(w http.ResponseWriter, r *http.Request, raw string) (canonical.Asset, bool) {
	if raw == "" {
		return defaultPriceQuote, true
	}
	q, err := canonical.ParseAsset(raw)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-quote",
			"Invalid quote identifier", http.StatusBadRequest,
			err.Error())
		return canonical.Asset{}, false
	}
	return q, true
}

// priceBatchConcurrency bounds the per-request fan-out in lookupPriceBatch (each id is a LatestPrice + fallback-chain round-trip; serial
// put p99 at the 10s ceiling).
// MEASURED; do not lower it as a throughput control: narrowing only lengthens connection hold times. The real bound on a 1000-id POST is
// the rate limiter.
const priceBatchConcurrency = 16

// batchRowResult is the per-id outcome computed by resolveBatchRow.
// Exactly one of {ok, skip, fail} characterises a result.
type batchRowResult struct {
	snap         PriceSnapshot
	sources      []string
	stale        bool
	frozen       bool
	triangulated bool
	asset        canonical.Asset

	ok   bool // a real price row — include in the envelope
	skip bool // per-asset miss — omit, do NOT 404 the batch
	// withheld qualifies a skip: a gate declined to publish a price the server holds, so the id
	// goes on the envelope's `withheld` list rather than reading as "no data".
	withheld bool
	// withheldReason keys the include_thin second pass.
	withheldReason PriceWithheldReason
	// thin: served under include_thin from a market below the substance floor; no change_24h_pct.
	thin bool
	// readFailed qualifies a skip: dropped because a read failed, not because there is no price.
	readFailed bool
	fail       *batchRowFailure
}

// batchRowFailure aborts the whole batch with a problem+json. It is a value because resolveBatchRow runs in a worker goroutine,
// where touching the shared http.ResponseWriter would race; the orchestrator writes the first failure.
type batchRowFailure struct {
	status int
	typ    string
	title  string
	detail string
}

// resolveBatchRowShared coalesces [Server.resolveBatchRow] across concurrent requests for the same (id, quote, include_thin):
// sharded explorer builds ask for the same cold 100-id chunks at once, and each duplicate re-ran the gates and fallback chain.
// The shared work keeps the leader's deadline (priceReadFlightTimeout if none) but not its cancellation, so one client
// hanging up can't fail the row for the rest.
func (s *Server) resolveBatchRowShared(ctx context.Context, r *http.Request, raw string, quote canonical.Asset) batchRowResult {
	key := raw + "/" + quote.String() + "/thin:" + strconv.FormatBool(includeThinRequested(r))
	v, _, _ := s.batchRowFlight.Do(key, func() (any, error) { //nolint:contextcheck // deliberately detached from the leader's cancellation, see above
		dl, ok := ctx.Deadline()
		if !ok {
			dl = time.Now().Add(priceReadFlightTimeout)
		}
		fctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), dl)
		defer cancel()
		return s.resolveBatchRow(fctx, r.WithContext(fctx), raw, quote), nil
	})
	row, _ := v.(batchRowResult)
	return row
}

// resolveBatchRow resolves one (raw_id, quote) pair, goroutine-safe (never touches w); returns ok (a row), skip (per-asset miss, omitted)
// or fail (aborts the batch).
func (s *Server) resolveBatchRow(ctx context.Context, r *http.Request, raw string, quote canonical.Asset) batchRowResult {
	asset, err := canonical.ParseAsset(raw)
	if err != nil {
		return batchRowResult{fail: &batchRowFailure{
			status: http.StatusBadRequest,
			typ:    "https://api.stellarindex.io/errors/invalid-asset-id",
			title:  "Invalid asset identifier",
			detail: raw + ": " + err.Error(),
		}}
	}
	if asset.Equal(quote) {
		return batchRowResult{fail: &batchRowFailure{
			status: http.StatusBadRequest,
			typ:    "https://api.stellarindex.io/errors/identity-price",
			title:  "Asset and quote are the same",
			detail: "price of an asset in itself is always 1; " + raw + " matches the quote",
		}}
	}
	offCtx, off := WithThinAdmission(ctx, asset, quote, false)
	row := s.readBatchRow(offCtx, r, asset, quote)
	if row.withheld && includeThinRequested(r) && off.ThinWithheld(row.withheldReason) {
		onCtx, on := WithThinAdmission(ctx, asset, quote, true)
		thin := s.readBatchRow(onCtx, r, asset, quote)
		if thin.ok && thin.snap.Price != "" {
			thin.thin = on.Admitted()
			row = thin
		}
	}
	// No derived change from a thin price (ADR-0018); computed on the final (possibly held) snapshot.
	if row.ok && !row.thin {
		row.snap.Change24hPct = s.batchChange24h(ctx, asset, quote, row.snap.Price)
	}
	return row
}

// readBatchRow is one pass of a batch row's read under ctx's [ThinAdmission].
func (s *Server) readBatchRow(ctx context.Context, r *http.Request, asset, quote canonical.Asset) batchRowResult {
	// Alias loop, as handlePrice: the literal `native` alone would return stale/empty while
	// /v1/price serves the fresh VWAP published under crypto:XLM.
	snap, sources, stale, served, err := s.readPriceWithAliasesServed(ctx, s.Prices, asset, quote)
	if errors.Is(err, ErrPriceWithheld) {
		// Omit and name it on `withheld`; do NOT run priceFallback, which would re-serve the
		// withheld market via the Redis/proxy side doors.
		return batchRowResult{skip: true, withheld: true, withheldReason: priceWithheldReason(err)}
	}
	if errors.Is(err, ErrPriceNotFound) {
		// Share /v1/price's fallback chain: skipping the stablecoin proxy would drop ids that return
		// 200 there. A withheld verdict from any leg goes on `withheld`, never into "no data".
		fb := s.priceFallback(ctx, asset, quote)
		fs, fsrc, fserved, ftri := fb.snap, fb.sources, fb.served, fb.triangulated
		if fb.ok {
			// Fallbacks sit below the closed-bucket contract, so stale; triangulated carries through
			// for parity with /v1/price. fserved is the freeze check's governing pair.
			return s.holdFrozenBatchRow(r, batchRowResult{
				snap: fs, sources: fsrc, stale: fb.stale, triangulated: ftri,
				asset: asset, ok: true,
			}, fserved, quote)
		}
		readFailed := fb.err != nil && !clientAborted(r, fb.err)
		if readFailed {
			s.logger.Warn("batch: fx fixing read failed", "err", fb.err, "asset", asset.String())
		}
		// A failed FX read omits the row without listing it as withheld.
		return batchRowResult{skip: true, withheld: fb.withheld != "", withheldReason: fb.withheld, readFailed: readFailed} // omit, do not 404 the batch
	}
	if err != nil {
		if clientAborted(r, err) {
			// Client gone: the orchestrator sees the cancelled context after join and writes nothing.
			return batchRowResult{skip: true}
		}
		s.logger.Error("LatestPrice (batch) failed",
			"err", err, "asset", asset.String(), "quote", quote.String())
		return batchRowResult{fail: &batchRowFailure{
			status: http.StatusInternalServerError,
			typ:    "https://api.stellarindex.io/errors/internal",
			title:  "Internal error",
		}}
	}
	if basis, ok := s.preferUSDAnchoredBasis(ctx, asset, quote, snap, sources); ok {
		basis.snap.Change24hPct = s.batchChange24h(ctx, asset, quote, basis.snap.Price)
		return s.holdFrozenBatchRow(r, batchRowResult{
			snap: basis.snap, sources: basis.sources, stale: basis.stale, triangulated: true,
			asset: asset, ok: true,
		}, basis.served, quote)
	}
	// Raw closed-1m read: normalize here; the priceFallback branch above is already normalized.
	s.normalizeRawPriceSnapshot(&snap, asset, quote)
	// A frozen row carries the held value, not the raw bucket just read.
	return s.holdFrozenBatchRow(r, batchRowResult{
		snap: snap, sources: sources, stale: stale, asset: asset, ok: true,
	}, served, quote)
}

// batchChange24h computes the trailing-24h percentage change for a USD-quoted batch row (anchor: the USD price 24h ago, as
// /v1/assets/{id}); nil on any failure, by design, so a missing change never costs the caller the price.
func (s *Server) batchChange24h(ctx context.Context, asset, quote canonical.Asset, price string) *string {
	if s.Change24h == nil || !quote.Equal(defaultPriceQuote) {
		return nil
	}
	then, err := s.Change24h.USDPrice24hAgo(ctx, asset)
	if err != nil {
		return nil
	}
	pct, err := pctChange(price, then)
	if err != nil {
		return nil
	}
	return &pct
}

// lookupPriceBatch resolves every id concurrently (bounded by priceBatchConcurrency) into an index-keyed slice, so output keeps first-
// occurrence order.
// Misses are omitted; the first validation/internal failure in input order aborts the batch with a problem+json.
func (s *Server) lookupPriceBatch(w http.ResponseWriter, r *http.Request, ids []string, quote canonical.Asset) {
	ctx := r.Context()
	results := make([]batchRowResult, len(ids))

	// Each worker writes its own results[i] slot (no lock); the semaphore caps concurrent round-trips.
	sem := make(chan struct{}, priceBatchConcurrency)
	var wg sync.WaitGroup
	for i, raw := range ids {
		wg.Add(1)
		go func(i int, raw string) {
			// Registered first so it unwinds LAST: wg.Done must run or wg.Wait() deadlocks.
			defer wg.Done()
			// middleware.Recoverer doesn't cover this goroutine. On panic results[i] stays ZERO, so
			// the id is DROPPED rather than served with a fabricated price.
			defer worker.Recover(s.logger, "api-price-batch-row")
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.resolveBatchRowShared(ctx, r, raw, quote)
		}(i, raw)
	}
	wg.Wait()

	// Client disconnected mid-batch: nothing to write. A server-side deadline
	// (middleware.RequestTimeout) is NOT a client abort and must fall through to the fail-scan, so
	// writeProblem can answer a retryable 503 instead of net/http's bodyless 200 (see clientAborted).
	if clientAborted(r, nil) {
		return
	}
	// First failure in INPUT order aborts: deterministic whatever the completion order.
	for i := range results {
		if f := results[i].fail; f != nil {
			writeProblem(w, r, f.typ, f.title, f.status, f.detail)
			return
		}
	}

	env := batchEnvelope(ids, results)
	env.Flags.ProxyDeviation = env.Flags.Triangulated && s.proxyDeviation(r.Context(), time.Now().UTC())
	env.Flags.Degraded = env.Flags.Degraded || batchDegraded(results)
	writeEnvelope(w, env)
}

// batchDegraded reports a row dropped by a failed read or recovered panic, not a genuine miss.
func batchDegraded(results []batchRowResult) bool {
	for i := range results {
		r := &results[i]
		if r.readFailed || (!r.ok && !r.skip && r.fail == nil) {
			return true
		}
	}
	return false
}

// batchEnvelope folds the per-id results (index-aligned with ids) into the envelope; each flag is
// the OR over served rows.
func batchEnvelope(ids []string, results []batchRowResult) Envelope {
	out := make([]PriceSnapshot, 0, len(ids))
	var withheld, thin, stale, triangulated []string
	allSources := map[string]struct{}{}
	anyFrozen := false
	anySingleSource := false
	for i := range results {
		row := results[i]
		if !row.ok {
			if row.withheld {
				withheld = append(withheld, ids[i])
			}
			continue // skip — per-asset miss
		}
		if row.stale {
			stale = append(stale, ids[i])
		}
		if row.triangulated {
			triangulated = append(triangulated, ids[i])
		}
		if row.thin {
			thin = append(thin, ids[i])
		}
		for _, src := range row.sources {
			allSources[src] = struct{}{}
		}
		if row.frozen {
			anyFrozen = true
			anySingleSource = true // freeze implies single-source
		} else if marketSingleSource(row.snap, row.sources) {
			anySingleSource = true
		}
		out = append(out, row.snap)
	}
	srcs := make([]string, 0, len(allSources))
	for src := range allSources {
		srcs = append(srcs, src)
	}
	// Sort for ADR-0015 byte-identical cross-region output: map iteration order would break it.
	sort.Strings(srcs)
	return Envelope{
		Data:         out,
		Sources:      srcs,
		Withheld:     withheld,
		Thin:         thin,
		Stale:        stale,
		Triangulated: triangulated,
		Flags: Flags{
			ThinMarket:   len(thin) > 0,
			Stale:        len(stale) > 0,
			Frozen:       anyFrozen,
			Degraded:     anyFrozen,
			SingleSource: anySingleSource,
			Triangulated: len(triangulated) > 0,
		},
	}
}

// ─── Helpers for PriceReader implementations ──────────────────────

// LastTradeToSnapshot converts a canonical.Trade to a last_trade PriceSnapshot (QuoteAmount / BaseAmount as a decimal string); ok is false for a zero leg.
func LastTradeToSnapshot(t canonical.Trade, decimals int) (PriceSnapshot, bool) {
	price, ok := priceRatioDecimal(t, decimals)
	if !ok {
		return PriceSnapshot{}, false
	}
	return PriceSnapshot{
		AssetID:    t.Pair.Base.String(),
		Quote:      t.Pair.Quote.String(),
		Price:      price,
		PriceType:  "last_trade",
		ObservedAt: WireTime(t.Timestamp),
	}, true
}

// VWAP1mToSnapshot maps a prices_1m row to a [PriceSnapshot]; `vwap` is unparsed NUMERIC text and observed_at is the END of the closed window.
func VWAP1mToSnapshot(assetID, quote, vwap string, bucketStart time.Time) PriceSnapshot {
	return PriceSnapshot{
		AssetID:       assetID,
		Quote:         quote,
		Price:         vwap,
		PriceType:     "vwap",
		ObservedAt:    WireTime(bucketStart.Add(60 * time.Second)),
		WindowSeconds: 60,
	}
}

// priceRatioDecimal returns QuoteAmount / BaseAmount (raw stroop units) as a decimal string via big.Rat, no float (ADR-0003); ok=false
// when either leg is not positive.
// It truncates, never rounds, and extends a ratio too small for `decimals` rather than serving zero ([ratToDecimal], so /v1/history and
// bucketed surfaces agree digit for digit).
func priceRatioDecimal(t canonical.Trade, decimals int) (string, bool) {
	base, quote := t.BaseAmount.BigInt(), t.QuoteAmount.BigInt()
	if base.Sign() <= 0 || quote.Sign() <= 0 {
		return "", false
	}
	return ratToDecimal(new(big.Rat).SetFrac(quote, base), decimals), true
}

func leftPad(s string, n int, c byte) string {
	buf := make([]byte, n+len(s))
	for i := 0; i < n; i++ {
		buf[i] = c
	}
	copy(buf[n:], s)
	return string(buf)
}

// parsePriceQuoteParam parses the optional ?quote= (default fiat:USD); ok=false means a 400 was written.
func parsePriceQuoteParam(w http.ResponseWriter, r *http.Request) (canonical.Asset, bool) {
	rawQuote := r.URL.Query().Get("quote")
	if rawQuote == "" {
		return defaultPriceQuote, true
	}
	quote, err := canonical.ParseAsset(rawQuote)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-quote",
			"Invalid quote identifier", http.StatusBadRequest,
			err.Error())
		return canonical.Asset{}, false
	}
	return quote, true
}
