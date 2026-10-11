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

// priceBatchMaxAssets is the upper bound on asset_ids per
// /v1/price/batch GET. Mirrors the OpenAPI spec.
const priceBatchMaxAssets = 100

// priceBatchMaxAssetsPOST is the upper bound on asset_ids per
// /v1/price/batch POST. The JSON-body variant exists precisely to
// raise the GET ceiling without bloating query strings.
const priceBatchMaxAssetsPOST = 1000

// DivergenceLooker consults cached divergence results when serving /v1/price (production: an
// adapter around `internal/divergence.Service.LookupCached`).
//
// Absence (`found=false`) and read errors both leave the warning unset; errors are logged at WARN,
// absence is the steady state for assets the worker hasn't reached.
type DivergenceLooker interface {
	// DivergenceFiringFor reports whether the divergence warning is firing for the EXACT
	// (asset, quote) pair. `checked` is true only when a cached result met the worker's own
	// min_sources_for_warning quorum, so a false `firing` with checked=false is not "prices agree".
	// The verdict is quote-specific: XLM/GBP's must never leak onto an XLM/USD response.
	// window is the aggregation window the verdict compared, 0 when the record names none.
	DivergenceFiringFor(ctx context.Context, asset, quote canonical.Asset) (firing, checked bool, window time.Duration, err error)
}

// FrozenLooker tells the v1 server whether the latest bucket for a pair was frozen by the anomaly
// checker (ADR-0019); production is a Redis adapter the aggregator populates at bucket close.
//
// FrozenForPair reports the MARKER only. LatestPrice reads the raw prices_1m bucket, which the
// checker never gates, so on a frozen pair that snapshot is the bucket the freeze refused;
// [Server.resolveFrozenServe] swaps in the held VWAP (or refuses) before setting flags.frozen and
// flags.single_source (anomaly.ActionFreeze contract).
//
// Read errors fall through with frozen=false: better a price without the flag than a 5xx on a
// Redis blip.
type FrozenLooker interface {
	FrozenForPair(ctx context.Context, asset, quote canonical.Asset) (bool, error)
}

// PriceReader is the storage-side interface for /v1/price lookups.
//
// Production: Redis hot path (`price:<asset>`, ADR-0007) with a Timescale fallback to the latest
// trade. Responses served from that fallback are flagged stale=true (docs/architecture/ha-plan.md
// section 9).
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

// ErrPriceNotFound is what PriceReader.LatestPrice returns when no
// data exists for the pair. Handler translates to HTTP 404
// problem+json.
var ErrPriceNotFound = errors.New("api: price not found for pair")

// ErrPriceWithheld is returned when the pair HAS data but the thin-market substance gate refuses
// to publish an aggregated price: trailing activity is below the serve floor, so any price would
// be attacker-authorable.
//
// Handlers map it to a 404 with the distinct type ".../price-withheld" so integrators can tell
// "never seen" from "seen, declined; read /v1/observations, /v1/ohlc or /v1/history yourself".
// The fallback chain is deliberately NOT consulted on a withheld read: it would re-serve the same
// substanceless market through a side door.
var ErrPriceWithheld = errors.New("api: price withheld — trailing market substance below serve floor")

// PriceWithheldReason discriminates WHY a price read was withheld (substance, scam-issuer, a
// propagated upstream leg), so a flagged issuer isn't described in the substance gate's wording.
// It travels alongside the sentinel: errors.Is(err, ErrPriceWithheld) still matches via Unwrap.
type PriceWithheldReason string

const (
	// PriceWithheldSubstance: the thin-market substance gate refused —
	// trailing market activity below the serve floor.
	PriceWithheldSubstance PriceWithheldReason = "substance"
	// PriceWithheldScamIssuer: a directory-flagged issuer sits on
	// either leg of the pair.
	PriceWithheldScamIssuer PriceWithheldReason = "scam_issuer"
	// PriceWithheldUpstreamLeg: a derived cross-rate's upstream leg
	// (e.g. the USD leg an FX cross is composed from) was itself
	// withheld, and the verdict propagated to the derived pair.
	PriceWithheldUpstreamLeg PriceWithheldReason = "upstream_leg"
	// PriceWithheldUnattributed: a withholding verdict that arrived
	// without its cause (a folded bool, or a bare ErrPriceWithheld).
	// Its wording claims neither cause.
	PriceWithheldUnattributed PriceWithheldReason = "unattributed"
	// PriceWithheldManipulationGuard: the serving-sanity guard could not
	// validate the bucket at a requested instant — it was a gross outlier
	// with no clean bucket inside the caller's staleness bound, or it had
	// no prior bucket to be judged against ([ErrPriceAtGuarded]).
	PriceWithheldManipulationGuard PriceWithheldReason = "manipulation_guard"
	// PriceWithheldFXLeg: a closed-bucket fiat cross found no vendor FX
	// fixing inside the lookback at its bucket end. The live rate is not a
	// substitute: it would make the closed answer depend on when it was read.
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

// PriceWithheldError is the ErrPriceWithheld a reader returns for a
// pricingguard verdict, carrying which gate fired to the response.
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

// withheldBy folds the server's two gate seams into one verdict, with
// pricingguard's precedence: the scam gate is asked FIRST and a flagged
// pair is withheld unmeasured. Nil gates withhold nothing. A covered thin
// leg is released under the request's [ThinAdmission]. It and
// scamWithheld are the package's only permitted gate-half calls
// (cmd/stellarindex-api TestWithholdingGatesAreSpelledOnlyAtTheChokepoint).
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

// PriceWithheldReasonOf is [priceWithheldReason] for producers outside
// this package that put a withholding on a stream.
func PriceWithheldReasonOf(err error) PriceWithheldReason { return priceWithheldReason(err) }

// PriceSubstanceGate is the serving-side thin-market gate seam (production:
// internal/pricingguard.SubstanceGate). The server consults it directly only on paths that skip
// PriceReader (the tip rolling-window VWAP); reader-backed paths are gated inside the readers.
//
// surface is a low-cardinality metric label ("tip", "price_read", ...).
//
// Allowed fails open when the pair cannot be measured. Probe reports that case as measured=false
// (allowed=false), uncounted, for the listing verdict that must not publish an unverified price;
// see [Server.listingSubstanceVerdict].
type PriceSubstanceGate interface {
	Allowed(ctx context.Context, base, quote canonical.Asset, surface string) bool
	Probe(ctx context.Context, base, quote canonical.Asset) (allowed, measured bool, floor pricingguard.SubstanceFloor)
}

// PriceSubstanceMeasurer is the optional evidence-carrying form of
// [PriceSubstanceGate]; production's *pricingguard.SubstanceGate has it.
// A gate without it is folded through Allowed and records no evidence.
type PriceSubstanceMeasurer interface {
	Measure(ctx context.Context, base, quote canonical.Asset) pricingguard.SubstanceVerdict
}

// PriceScamGate withholds the aggregated price for an asset whose ISSUER is flagged scam-class
// in the curated directory (production: internal/pricingguard.ScamGate). Like the substance gate,
// the server consults it only on paths that skip PriceReader. Nil-safe: a nil gate withholds
// nothing.
//
// This is the BASE-ONLY question, half the decision (see [PriceScamPairGate]); handlers must ask
// via [scamWithheld], never call this method directly.
type PriceScamGate interface {
	Withheld(ctx context.Context, base canonical.Asset, surface string) bool
}

// PriceScamPairGate is the pair-aware form of [PriceScamGate]: it withholds when EITHER leg's
// issuer is flagged.
//
// Withholding is a property of the MARKET, not of whichever leg the client named first: the price
// of X in a flagged issuer's asset IS the flagged market's price, inverted. A base-only gate would
// serve `?base=native&quote=<FLAGGED>` at 200 as the reciprocal of the number withheld for the
// opposite orientation.
//
// A separate interface (not a second method) so a deployment wiring a base-only gate keeps its
// base-only behaviour instead of silently satisfying a widened contract. The production gate
// implements this form, pinned by TestProductionScamGateIsPairAware so the fallback can never
// become the live path.
type PriceScamPairGate interface {
	PriceScamGate
	WithheldPair(ctx context.Context, base, quote canonical.Asset, surface string) bool
}

// scamWithheld is the ONE spelling of the scam decision in this package: every handler asks it
// with BOTH legs and the fold over legs happens inside pricingguard. A hand-written
// `gate.Withheld(base) || gate.Withheld(quote)` per handler drifts per site (it once let /v1/vwap
// and /v1/twap serve a flagged issuer's price that /v1/price withheld).
//
// The type assertion is a migration seam: gates without the pair form answer base-only.
func scamWithheld(ctx context.Context, gate PriceScamGate, base, quote canonical.Asset, surface string) bool {
	if gate == nil {
		return false
	}
	if pair, ok := gate.(PriceScamPairGate); ok {
		return pair.WithheldPair(ctx, base, quote, surface)
	}
	return gate.Withheld(ctx, base, surface)
}

// writeIfScamWithheld is [scamWithheld] for a handler that answers the
// verdict itself: it asks the pair question and, when the gate
// withholds, writes the problem with the scam reason. Binding the two
// is the point — a handler that gated on the flag and wrote the
// reason-free problem told the client the market was too thin and to
// recompute the price from the raw trades.
func (s *Server) writeIfScamWithheld(w http.ResponseWriter, r *http.Request, base, quote canonical.Asset, surface string) bool {
	if !scamWithheld(r.Context(), s.Scam, base, quote, surface) {
		return false
	}
	writePriceWithheldProblem(w, r, base, quote, PriceWithheldScamIssuer)
	return true
}

// writePriceWithheldProblem is the single serializer for the withheld
// verdict: one problem type (clients branch on it), wording chosen by
// the gate that fired. Only the thin-market verdict points the reader
// at the raw surfaces to judge the market themselves — a flagged
// issuer's trades are not a price signal to recompute, and a verdict
// whose cause is unknown or inherited may be exactly that.
func writePriceWithheldProblem(w http.ResponseWriter, r *http.Request, asset, quote canonical.Asset, reason PriceWithheldReason) {
	writePriceWithheldProblemEvidence(w, r, asset, quote, reason, nil)
}

// writePriceWithheldProblemEvidence is [writePriceWithheldProblem] with
// the substance measurement behind the verdict (nil: none). The wording
// never carries the numbers; the `substance` member does.
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

// writeNoPriceProblem emits the 404 for an exhausted price lookup: WITHHELD when some leg reached
// a withholding verdict, plain not-found otherwise. The distinction matters because "no price
// data" says "we have none" when the truth may be "we have one and decline to publish it".
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

// defaultPriceQuote is the implicit `quote` used by /v1/price when
// the client omits the query param. Parsed once at package init
// so a regression that removes USD from the fiat allow-list
// produces a loud init panic — instead of silently 400ing every
// no-quote /v1/price request in production.
var defaultPriceQuote = mustParseAsset("fiat:USD")

func mustParseAsset(s string) canonical.Asset {
	a, err := canonical.ParseAsset(s)
	if err != nil {
		panic("api/v1: defaultPriceQuote " + s + " must parse: " + err.Error())
	}
	return a
}

// PriceSnapshot is the neutral shape returned by [PriceReader]. The
// handler wraps it in [Envelope].
type PriceSnapshot struct {
	// AssetID + Quote canonical strings match the request parameters.
	AssetID string `json:"asset_id"`
	Quote   string `json:"quote"`

	// Price as a decimal string — ADR-0003 forbids float here.
	// Computed by the reader from the underlying trade or CAGG row.
	Price string `json:"price"`

	// PriceType is one of "vwap", "twap", "last_trade", "peg"; the reader picks the best available
	// (VWAP > TWAP > last_trade). "peg" (tryStablecoinFiatProxy) is the ONLY value that is not an
	// observation: the direct fiat:USD read AND the XLM cross both missed, and the answer is the
	// operator's standing 1:1 declaration.
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

	// Substituted is true when the serving-sanity guard rejected the latest closed bucket as an
	// outlier and served an older last-known-good one. Internal (not on the wire): it tells the
	// handler NOT to staple confidence/composite enrichment read under the CURRENT tick's Redis
	// keys, which carry no as-of and would misattribute a live read to a stale value. See
	// [Server.attachConfidence] / [Server.attachCompositeFlags].
	Substituted bool `json:"-"`

	// usdLeg is set only on an ADR-0051 cross
	// ([Server.tryUSDAnchoredFiatCross]); see [usdLegFacts].
	usdLeg *usdLegFacts

	// displacedBook marks an ADR-0053 basis serve: the pair's own book was
	// read and replaced, so its pair-keyed confidence and flags describe a
	// value that is not on the wire.
	displacedBook bool
}

// USDLeg is the USD price a derived fiat price was converted from.
type USDLeg struct {
	Price      string   `json:"price"`
	ObservedAt WireTime `json:"observed_at"`
	Sources    []string `json:"sources"`
}

// usdLegFacts is what an ADR-0051 cross inherits from its USD leg. The
// FX rate converts one market observation; it is not a second market,
// and the pair the aggregator freezes is the USD leg, never the derived
// fiat pair it has no key for.
type usdLegFacts struct {
	sources []string        // the leg's own venues, without the FX credit
	served  canonical.Asset // alias the leg's bucket was read under; zero when a fallback answered
}

// marketSingleSource is the single_source rule shared by every price
// surface: exactly one market venue stands behind the value. An FX
// conversion credited in sources[] does not count as a venue.
func marketSingleSource(snap PriceSnapshot, sources []string) bool {
	if leg := snap.usdLeg; leg != nil {
		return len(leg.sources) == 1
	}
	return len(sources) == 1
}

// ConfidenceFactors mirrors `confidence.Factors` on the wire so
// the API package doesn't force an aggregate import on SDK
// consumers. Same field names and JSON tags.
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

	// TriangulationAgreement is the seventh confidence factor (ADR-0019): agreement between the
	// DIRECT price and the COMPOSITE its triangulation chain implies, shaped like cross_oracle
	// (1.0 = agrees, halving per 4pp beyond a 2% tolerance, 0.7 no-data neutral).
	// TriangulationChecked=false means no chain or no fresh composite, NOT "the composite
	// agrees"; the factor then carries weight 0, so the score is as if it did not exist.
	TriangulationAgreement float64 `json:"triangulation_agreement"`
	TriangulationChecked   bool    `json:"triangulation_checked"`

	// BaselineAgeDays is the 30d baseline DENSITY the bootstrap cap gates on, in days-equivalent
	// of 1-minute buckets (at most 30; negative = no usable baseline), not calendar age.
	// BootstrapCapped is true when the ADR-0019 ceiling (0.5) bounded confidence because density
	// is under 28.5, so 0.5 may be the cap rather than the evidence.
	BaselineAgeDays float64 `json:"baseline_age_days"`
	BootstrapCapped bool    `json:"bootstrap_capped"`
}

// ConfidenceLooker fetches the cached confidence score for `/v1/price` (production: a Redis
// adapter over `confidence:<base>:<quote>:<window>`).
//
// Absent entries return `(_, false, nil)`; read errors propagate via the third return so the
// handler can log without failing, since confidence is enrichment, not a publish-blocking signal.
type ConfidenceLooker interface {
	LookupConfidence(ctx context.Context, asset, quote canonical.Asset, window time.Duration) (PriceSnapshotConfidence, bool, error)
}

// PriceSnapshotConfidence is the read-side wire shape between
// [ConfidenceLooker] and the handler. Same fields as the
// orchestrator-side `confidence.Score` but local to this package
// so the storage adapter does the JSON decode + remap once.
type PriceSnapshotConfidence struct {
	Confidence float64
	Factors    ConfidenceFactors
}

// TriangulatedPriceLooker is the fallback /v1/price consults after a Timescale miss. The
// aggregator publishes implied VWAPs (e.g. XLM/EUR via XLM/USD x USD/EUR) to Redis with a
// provenance sibling set to "triangulated"; this reads both (via [cachekeys.VWAP],
// [cachekeys.VWAPProvenance], [cachekeys.VWAPObservedAt]). Nil leaves triangulated-only pairs 404.
type TriangulatedPriceLooker interface {
	// LookupTriangulatedVWAP returns the cached VWAP for the pair +
	// window. found is true when a value AND its observed-at stamp
	// were in the cache; a value without a readable stamp is a miss,
	// since nothing else can say how old it is. Cache misses are NOT
	// errors (found=false, err=nil); err propagates Redis errors so
	// the handler can log them.
	LookupTriangulatedVWAP(ctx context.Context, base, quote canonical.Asset, window time.Duration) (vwap CachedVWAP, found bool, err error)
}

// lookupCachedVWAP is the one read of the aggregator's VWAP cache every
// serve path goes through. It refuses a value whose looker reported no
// observation time: stamping one would be inventing it.
func (s *Server) lookupCachedVWAP(ctx context.Context, base, quote canonical.Asset, window time.Duration) (CachedVWAP, bool, error) {
	v, found, err := s.Triangulated.LookupTriangulatedVWAP(ctx, base, quote, window)
	if err != nil || !found || v.ObservedAt.IsZero() {
		return CachedVWAP{}, false, err
	}
	return v, true, nil
}

// lookupCachedVWAPAliased is [Server.lookupCachedVWAP] walked over every
// (base, quote) spelling in the given priority order, returning the first
// hit and the literal pair it was read under. The aggregator publishes a
// value under ONE spelling (the GBP composite only as crypto:XLM/fiat:GBP),
// so a literal-only read makes the answer depend on how the client spelled
// the asset. Callers key every per-pair marker on the returned pair.
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

// CompositeMetaLooker is an OPTIONAL capability a [TriangulatedPriceLooker] may implement to
// expose the router-quality flags persisted at [cachekeys.VWAPCompositeMeta]: `diverged`
// (contributing routes disagreed) and `rerouted` (a dry chain leg was substituted around).
//
// Optional (the [proxyPairGate] idiom) so a looker without it degrades to "flags unset"; the
// flags are enrichment on an already-served price. Best-effort: a miss, malformed blob or read
// error leaves them unset and NEVER fails the request.
type CompositeMetaLooker interface {
	// LookupCompositeMeta returns the raw JSON meta blob the aggregator
	// wrote under [cachekeys.VWAPCompositeMeta] for the (base, quote,
	// window) composite. found=false on a cache miss (not an error);
	// err carries a real read failure for logging only.
	LookupCompositeMeta(ctx context.Context, base, quote canonical.Asset, window time.Duration) (raw []byte, found bool, err error)
}

// ─── Handler ──────────────────────────────────────────────────────

// resolveAssetOrBaseParam reads `asset` (canonical) or its alias `base` on /v1/price,
// /v1/observations and /v1/chart, so URLs copied from /v1/twap don't 400. Passing both is a 400
// to avoid silent precedence. The dual is `parseBaseQuote` in history.go.
//
// Returns (raw, true), or writes a 400 Problem and returns ok=false on missing/conflicting params.
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

// parsePriceAssetParam preserves the historic name for /v1/price's
// handler while delegating to the shared `asset=`-canonical resolver.
func parsePriceAssetParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	return resolveAssetOrBaseParam(w, r)
}

// parsePricePairParams is handlePrice's parameter prologue — asset
// resolution (asset=/base= alias), quote defaulting, and the
// identity-pair rejection — extracted verbatim to keep the handler
// under the gocognit budget. ok=false means a problem+json has been
// written.
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

// handlePrice serves GET /v1/price?asset=<id>&quote=<id>; `quote` defaults to "fiat:USD".
//
// It serves the last CLOSED 1-minute VWAP bucket, never an in-progress one (ADR-0015), so the
// answer is byte-identical across regions with the same ledgers, at ~30-120 s staleness by
// construction. `?window=` is rejected (ADR-0018): a query parameter must not select between
// consistency surfaces. Rolling windows belong on /v1/price/tip, history on /v1/history.
//
// `flags.stale` means the closed bucket was unavailable and this degraded to a last-trade
// fallback, or the pair is frozen and the held value is served (see [Server.resolveFrozenServe]),
// or the sanity guard held an older bucket in place of an outlier. This surface deliberately
// differs from /v1/price/tip and /v1/assets price_usd (see "Current-price surfaces" in the
// package doc).
func (s *Server) handlePrice(w http.ResponseWriter, r *http.Request) {
	// ?window= would select between the closed-bucket surface and the rolling VWAP, which ADR-0018
	// prohibits. Rejected before the reader-nil check so a misconfigured deployment 400s like a
	// healthy one instead of leaking its failure mode.
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

	// Try the literal (asset, quote) first, then known aliases: the aggregator writes VWAP under
	// whichever XLM form its pair set names, so without this loop /v1/price?asset=native misses a
	// fresh `crypto:XLM/fiat:USD` VWAP and falls to triangulation (ADR-0010).
	ps, adm := s.resolvePriceServeThin(r, reader, asset, quote)
	// Withheld beats every fallback: the substance gate refused to
	// publish an aggregated price for this pair, and the fallback chain
	// (Redis VWAP / stablecoin proxy / cross-rate) would just re-serve
	// the same substanceless market through a side door. Distinct 404
	// type so integrators can branch (see ErrPriceWithheld).
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

	// A frozen pair serves the value the freeze is HOLDING, never the
	// one just read — see [Server.resolveFrozenServe]. The held value
	// comes from the aggregator's VWAP cache, so it takes the
	// viaFallback treatment: already normalized upstream, and below
	// this surface's closed-bucket baseline (so stale).
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

// priceServe is one pass of /v1/price's read: the closed-bucket read,
// then the fallback chain on a not-found. Exactly one of a price, miss or
// err (an ErrPriceWithheld when the direct read was withheld) is set.
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

// resolvePriceServe runs one pass under whatever [ThinAdmission] ctx
// carries.
func (s *Server) resolvePriceServe(ctx context.Context, reader PriceReader, asset, quote canonical.Asset) priceServe {
	// Try the literal (asset, quote) first, then known aliases: the aggregator writes VWAP under
	// whichever XLM form its pair set names, so without this loop /v1/price?asset=native misses a
	// fresh `crypto:XLM/fiat:USD` VWAP and falls to triangulation (ADR-0010).
	snapshot, sources, stale, served, err := s.readPriceWithAliasesServed(ctx, reader, asset, quote)
	if errors.Is(err, ErrPriceNotFound) {
		fb := s.priceFallback(ctx, asset, quote)
		if !fb.ok {
			return priceServe{miss: &fb}
		}
		// Every fallback sits below the surface's closed-bucket contract,
		// so flags.stale is the chain's own verdict, except a
		// closed fiat cross, which reports its own legs' staleness.
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

// resolvePriceServeThin is the two-pass driver: the default read first,
// and only when it ended withheld on a measured thin leg and the request
// opted in, the same read once more with that leg admitted. A cleared route
// therefore always wins over a thin one. The returned record is the one
// whose pass is served.
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

// handlePriceTail continues handlePrice — normalization, confidence/flags
// assembly and the response write — split out purely to keep handlePrice
// under the funlen ceiling as its flag set has grown (same reason
// [registerAppMetricsTail] was peeled off registerAppMetrics).
func (s *Server) handlePriceTail(w http.ResponseWriter, r *http.Request, asset, quote, served canonical.Asset, snapshot PriceSnapshot, sources []string, stale, triangulated, viaFallback, frozen, frozenChecked bool, adm *ThinAdmission) {
	// dex-nonstandard-decimals forward normalization (see
	// docs/operations/runbooks/dex.md): only when the snapshot came from
	// the RAW closed-1m prices_1m bucket read (or its raw last-trade
	// fallback inside LatestPrice) — not from priceFallback, whose Redis-
	// VWAP branch is already normalized upstream by the orchestrator
	// (internal/aggregate/orchestrator.go computeNormalizedVWAP) and
	// whose peg/cross-rate branches never touch a flagged Soroban leg.
	if !viaFallback {
		s.normalizeRawPriceSnapshot(&snapshot, asset, quote)
	}

	// Do NOT emit obs.PriceStalenessSeconds here: one series per queried asset is a cardinality
	// blowup on Stellar. The aggregator owns it (orchestrator emitStalenessGauges, bounded VWAP set).

	// Confidence is best-effort enrichment (ADR-0019), looked up only on `/v1/price`: misses and
	// read errors leave Confidence/ConfidenceFactors nil. Skipped on a substituted snapshot, see
	// [Server.attachConfidence], and on a displaced book, whose pair keys describe the replaced value.
	pairKeyed := !snapshot.Substituted && !snapshot.displacedBook
	if pairKeyed {
		s.attachConfidence(r, &snapshot, asset, quote, heldWindow(frozen, snapshot, confidenceLookupWindow))
	}

	// Every per-pair marker below is asked for the spelling the price was
	// served from (frozenPairBase's resolution), not the one the client
	// used: the aggregator keys each on the literal pair it priced.
	governing := served
	if governing.IsZero() {
		governing = asset
	}
	flags := Flags{Stale: stale, Triangulated: triangulated, ProxyDeviation: triangulated && s.proxyDeviation(r.Context(), time.Now().UTC())}
	// Surface the router's composite-quality signals (diverged / rerouted) from
	// cachekeys.VWAPCompositeMeta. Runs whichever arm served, since the router tracks the chain
	// even when a real closed bucket wins; a cache miss is a no-op. Best-effort. Skipped on a
	// substituted snapshot, see [Server.attachCompositeFlags].
	if pairKeyed {
		s.attachCompositeFlags(r, &flags, governing, quote, heldWindow(frozen, snapshot, triangulationLookupWindow), false)
	}
	flags.Frozen = frozen
	flags.FrozenChecked = frozenChecked
	flags.Degraded = frozen
	// SingleSource is forced true on the LKG fallback (ActionFreeze: a multi-source bucket
	// couldn't have been frozen). Otherwise it derives from the source count, except when a
	// WIRED freeze looker's read failed: freeze status is then UNKNOWN, not confirmed-false, so
	// deriving it would assert a fact the read never established. With no looker wired there is
	// no verdict to be wrong about.
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

// triangulationLookupWindow is the Redis VWAP window the API reads: 5m is the orchestrator's
// smallest default window ([orchestrator.DefaultWindows]), the one both the direct refresh and
// the triangulator write on every tick. No writer emits a 1m key.
//
// The served `observed_at` is the aggregator's own stamp ([cachekeys.VWAPObservedAt]), not the
// read time: a freeze keeps a value in the cache for its whole hold without rewriting it.
const triangulationLookupWindow = 5 * time.Minute

// tryRedisVWAPFallback consults the [TriangulatedPriceLooker] after a Timescale miss on
// /v1/price. It covers triangulated values AND stablecoin-fiat-proxy rewrites, neither of which
// appears in `prices_1m` (the CAGG groups by literal trade pair, not the rewritten target).
//
// The provenance marker sets `flags.triangulated`; marker-absent hits are accepted because
// Timescale is the source of truth only for LITERAL pairs.
//
// ObservedAt is the aggregator's stamp, never the read time; Sources is empty (Redis VWAP keys
// carry no provenance). Staleness is the caller's call (ADR-0018). The read walks the BASE's
// [assetAliases] against the literal quote like [Server.readPriceWithAliases]; served is the
// base alias the value was read under.
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

// assetAliases is the api/v1 spelling of [canonical.AssetAliases] —
// the single primitive that knows XLM's three canonical identities
// (`native`, `crypto:XLM`, and the XLM SAC C-address) and the priority
// order the read paths must try them in. See that godoc for why the SAC
// form is last (a thin Soroban pool must not outrank SDEX + CEX depth)
// and for the classic↔SAC registry this deliberately does NOT attempt.
// The primitive's contract test lives in canonical, beside it.
func assetAliases(asset canonical.Asset) []canonical.Asset {
	return canonical.AssetAliases(asset)
}

// distinctMarkets drops any pair whose flip already appeared, keeping the first spelling of each
// market and its position.
//
// [timescale.Store.TradesInRange] and [timescale.Store.OHLCSeries] read a market in BOTH stored
// directions, so merging a pair with its flip would count every trade twice. This is reachable:
// `?asset=native&quote=crypto:XLM` makes [tipMergePairs] emit both orientations.
//
// Dropping rather than reordering keeps the classic-before-SAC priority: the survivor is the
// spelling the caller's own walk reached first.
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

// sameAsset reports whether two canonical forms name the SAME asset: an identical spelling or two
// members of one alias family (`native`, `crypto:XLM`, the XLM SAC; a classic and its configured
// SAC wrapper), compared on the stable [canonical.CanonicalAsset] fold key.
//
// The proxy walks build one pair per (base, proxy-quote) combination, and a self-pair is not a
// market: it has no rows and would spend a slot of the shared deadline.
func sameAsset(a, b canonical.Asset) bool {
	return canonical.CanonicalAsset(a).Equal(canonical.CanonicalAsset(b))
}

// usdPegProxyQuotes is the operator's `[trades].usd_pegged_classic_assets` walk order with every
// canonical form of each peg folded in, in two passes: the classic form of EVERY declared peg in
// config order, then the remaining forms (SAC wrappers) in that order. Duplicates are dropped.
//
// A declared peg is an ASSET, not a spelling: Soroban AMMs trade the SAC wrapper, so a token whose
// USD depth is a Soroban pool is quoted in the USDC SAC, never `USDC-GA5Z...`.
//
// Two passes because walks take the first form that answers: every classic peg must stay ahead of
// every SAC form, or peg A's thin Soroban pool would be read before peg B's deep classic book (the
// SAC-LAST ordering of [canonical.AssetAliases], applied across the whole peg list).
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
	// Pass 1: the family's priority-first form of each peg — the classic
	// spelling however the operator wrote the entry.
	for _, peg := range s.USDPeggedClassics {
		add(canonical.CanonicalAsset(peg))
	}
	// Pass 2: the rest of each family, SAC wrapper last within it.
	for _, peg := range s.USDPeggedClassics {
		for _, form := range assetAliases(peg) {
			add(form)
		}
	}
	return out
}

// echoRequestedAsset restamps a snapshot's AssetID with the asset the CLIENT asked for, not
// whichever alias ([canonical.AssetAliases]) the store was keyed under.
//
// /v1/price/batch omits misses, so clients key the response by `asset_id`; without the restamp,
// asking for both `native` and `crypto:XLM` returns two rows with an IDENTICAL asset_id and
// `native` vanishes from the client's map. This is the contract [VWAP1mToSnapshot] states.
func echoRequestedAsset(snap PriceSnapshot, requested canonical.Asset) PriceSnapshot {
	snap.AssetID = requested.String()
	return snap
}

// readPriceWithAliases is the alias-aware read of reader.LatestPrice,
// coalesced through [Server.readPriceWithAliasesServed] and without the
// served alias. It tries each [assetAliases] form of asset against quote
// in order and returns the first fresh hit; when every hit is stale, the
// first stale one with stale=true; when every alias errors, the first
// withheld error, else the first error. The snapshot echoes the requested
// asset id. A bare LatestPrice(native, fiat:USD) would read only the
// `native` key and miss the `crypto:XLM` VWAP that CEX trades populate.
func (s *Server) readPriceWithAliases(ctx context.Context, reader PriceReader, asset, quote canonical.Asset) (PriceSnapshot, []string, bool, error) {
	snap, srcs, stale, _, err := s.readPriceWithAliasesServed(ctx, reader, asset, quote)
	return snap, srcs, stale, err
}

// priceReadResult carries readPriceWithAliasesServedOnce's return tuple
// through singleflight.Group.Do, which only carries one value.
type priceReadResult struct {
	snap   PriceSnapshot
	srcs   []string
	stale  bool
	served canonical.Asset
	// thinAdmitted and evidence are the flight's own [ThinAdmission]
	// answer, set on the error path too so a 404 body can carry evidence.
	thinAdmitted bool
	evidence     *pricingguard.SubstanceEvidence
}

// priceReadFlightTimeout bounds a coalesced price read. It runs on a context detached from any
// single caller (as lakeWMFlight in server.go does), so one caller's cancellation can't cut the
// read short for every other request waiting on the same (asset, quote) key.
const priceReadFlightTimeout = 8 * time.Second

// readPriceWithAliasesServed is [Server.readPriceWithAliasesServedOnce] coalesced via singleflight
// per (asset, quote), so a burst for a hot pair drives ONE upstream read.
//
// ctx is read only for the caller's [ThinAdmission]: the opt-in bit is part of the flight key, so a
// default caller never shares a flight that released a thin market, and each flight records into its
// own record.
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

// readPriceWithAliasesServedOnce is [Server.readPriceWithAliases] plus WHICH alias's market the
// snapshot was read from (the snapshot still echoes the requested id). The freeze marker is keyed
// on the literal pair the aggregator prices, so "is the pair I am about to serve frozen?" needs the
// pair actually read; see [Server.resolveFrozenServe]. The served alias is zero when err != nil.
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
				// Keep the reader's own error rather than manufacturing
				// a bare ErrPriceWithheld: if the reader has adopted
				// newPriceWithheld its reason survives to the response;
				// otherwise this is the same bare sentinel as before.
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
		// Stale — remember the first stale result as a fallback
		// in case every alias is stale.
		if !freshFound {
			firstSnap, firstSrcs, firstServed = echoRequestedAsset(snap, asset), srcs, a
			freshFound = true
		}
	}
	if freshFound {
		// Every alias was stale; return the first stale result so
		// the caller still surfaces flags.stale=true rather than
		// falling all the way through to priceFallback.
		return firstSnap, firstSrcs, true, firstServed, nil
	}
	// Every alias errored. A withheld verdict on ANY alias wins over not-found: surfacing
	// not-found would send the caller into priceFallback, re-serving the withheld market through
	// the Redis/proxy side doors. (The gate measures the ALIAS UNION, so a pair healthy under a
	// sibling alias was already allowed above.)
	if firstWithheldErr != nil {
		return PriceSnapshot{}, nil, false, canonical.Asset{}, firstWithheldErr
	}
	// Otherwise return the first error so the caller's
	// errors.Is(err, ErrPriceNotFound) branch still triggers
	// priceFallback as before.
	return PriceSnapshot{}, nil, false, canonical.Asset{}, firstErr
}

// normalizeRawPriceSnapshot applies the nonstandard-decimals forward normalization
// (aggregate.AdjustPrice) to a [PriceSnapshot] sourced from a RAW quote/base ratio: the closed-1m
// prices_1m read or its last-trade fallback. A post-hoc scalar multiply on the finished ratio is
// exact (docs/operations/runbooks/dex.md "Root cause analysis").
//
// Callers MUST NOT invoke this on a priceFallback snapshot: every branch is ALREADY normalized at
// its own source, so it would double-correct.
//
// No-op (price byte-identical, no parse/reformat round-trip) when baseDecimals == quoteDecimals:
// the CAGG's NUMERIC::text rendering differs from [ratToDecimal]'s fixed 10 digits, so reformatting
// unconditionally would change wire bytes for every already-correct 7dp pair.
func (s *Server) normalizeRawPriceSnapshot(snap *PriceSnapshot, base, quote canonical.Asset) {
	snap.Price = s.normalizeRawRatioString(snap.Price, base, quote)
}

// NormalizeRawPriceSnapshot is [Server.normalizeRawPriceSnapshot] for a
// caller with no *Server — streampublish's SSE bridge reads the same raw
// storePriceReader.LatestPrice source this method's doc comment names, but
// lives in its own package (avoiding an import cycle into v1), so it has no
// *Server to invoke the method through. decimals may be nil (fails open at
// aggregate.StandardDecimals, same as every other caller of ResolveDecimals).
func NormalizeRawPriceSnapshot(snap *PriceSnapshot, base, quote canonical.Asset, decimals *NonstandardDecimalsCache) {
	snap.Price = normalizeRawRatioStringWithLookup(snap.Price, base, quote, decimals)
}

// normalizeRawRatioString is the string-level primitive under [Server.normalizeRawPriceSnapshot],
// for raw-ratio paths that carry a bare price string (/v1/price/at, /v1/price/changes).
//
// base/quote MUST be the ACTUAL legs the ratio is priced in: for a stablecoin-proxy read that is
// asset/<peg>, NOT the requested fiat:USD.
//
// Returns value BYTE-IDENTICAL when the decimals match or value doesn't parse, so already-correct
// 7dp prices keep their wire bytes.
func (s *Server) normalizeRawRatioString(value string, base, quote canonical.Asset) string {
	return normalizeRawRatioStringWithLookup(value, base, quote, s.NonstandardDecimals)
}

// normalizeRawRatioStringWithLookup is the lookup-parameterized primitive
// under [Server.normalizeRawRatioString] and [NormalizeRawPriceSnapshot] —
// the single implementation both share.
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

// priceFallback runs the post-Timescale-miss fallback chain for /v1/price, in order:
//
//  1. Redis VWAP cache (triangulated chains + stablecoin proxy rewrites).
//  2. Read-time stablecoin -> fiat:USD rewrite against the operator-declared classic USD-pegs.
//  3. Fiat-vs-fiat cross-rate from the forex snapshot (always triangulated=true).
//
// Returns ok=false when every layer misses (the caller 404s). served is the base alias layer 1
// read under (the pair a freeze-aware caller asks its markers about), zero otherwise.
//
// The chain is entered on ErrPriceNotFound, whose exits are UNGATED (the priceWithheld chokepoint
// only runs on LatestPrice arms that produced a value), so the scam decision is asked HERE.
func (s *Server) priceFallback(ctx context.Context, asset, quote canonical.Asset) fallbackResult {
	// Scam-issuer gate, before layer 1: the cached VWAP is written without directory
	// consultation, so a flagged issuer's stablecoin-only market would be served at 200 here
	// while the reader gate withholds it. Layers 2-4 read through the GATED LatestPrice, so one
	// ask at the entry covers every layer and caller.
	//
	// SCAM ONLY, not the substance gate (as /v1/twap and /v1/vwap): the substance floor is
	// measured on the alias union, and a triangulated pair has zero literal rows BY CONSTRUCTION,
	// so asking it here would withhold the whole triangulated long tail. Both legs are asked via
	// [scamWithheld]: the market is the subject.
	if scamWithheld(ctx, s.Scam, asset, quote, "price_read") {
		return fallbackResult{withheld: PriceWithheldUnattributed}
	}
	if snap, srcs, served, triangulated, ok := s.tryRedisVWAPFallback(ctx, asset, quote); ok {
		return fallbackResult{snap: snap, sources: srcs, served: served, triangulated: triangulated, ok: true, stale: true}
	}
	snap, srcs, ok, withheld := s.tryStablecoinFiatProxy(ctx, asset, quote)
	if ok {
		return fallbackResult{snap: snap, sources: srcs, triangulated: true, ok: true, stale: true}
	}
	// Last, the fiat crosses: fiat/fiat, or the USD-anchored cross for a
	// NON-fiat asset quoted in a fiat we have no market for (ADR-0051). A
	// direct book that exists is weighed by [Server.preferUSDAnchoredBasis].
	var cross fallbackResult
	if asset.Type == canonical.AssetFiat {
		cross = s.closedFiatCross(ctx, asset, quote)
	} else {
		cross = s.closedUSDAnchoredFiatCross(ctx, asset, quote)
	}
	if cross.ok || cross.err != nil || cross.withheld != "" {
		return cross
	}
	// Nothing served. `withheld` distinguishes "we have no price" from
	// "we have one and decline to publish it" — the caller emits
	// errors/price-withheld rather than errors/price-not-found.
	if withheld {
		return fallbackResult{withheld: PriceWithheldUnattributed}
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
	// stale is flags.stale for the value: every fallback sits below the
	// closed-bucket contract except a closed fiat cross, which carries its
	// legs' own staleness.
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

// closedUSDAnchoredFiatCross prices a NON-fiat asset in a fiat for the closed surfaces:
// price(asset, USD) x the fixing binding at the USD leg's bucket end (ADR-0051, ADR-0018), so the
// answer never depends on when or where it is read.
//
// The USD leg resolves first so a withheld leg propagates before any FX read; multiplying a
// withheld price by a rate would publish it through an ungated door.
func (s *Server) closedUSDAnchoredFiatCross(ctx context.Context, asset, quote canonical.Asset) fallbackResult {
	if asset.Type == canonical.AssetFiat || quote.Type != canonical.AssetFiat || quote.Code == "USD" {
		return fallbackResult{}
	}
	usdSnap, usdSources, usdServed, usdStale, ok, withheld := s.resolveUSDLeg(ctx, asset)
	if withheld {
		return fallbackResult{withheld: PriceWithheldUnattributed}
	}
	if !ok || s.fxFixings == nil || isDeclaredPeg(usdSnap) {
		return fallbackResult{}
	}
	e := time.Time(usdSnap.ObservedAt)
	bound, err := s.bindFXFixings(ctx, []string{quote.Code}, e)
	if miss, done := fxBindFailure(err); done {
		return miss
	}
	b := bound[quote.Code]
	out, ok := convertAtFixing(usdSnap, usdSources, quote, b)
	if !ok {
		return fallbackResult{withheld: PriceWithheldFXLeg}
	}
	out.AssetID = asset.String()
	out.usdLeg = &usdLegFacts{sources: usdSources, served: usdServed}
	return fallbackResult{
		snap: out, sources: appendFXSource(usdSources, b.Source), triangulated: true, ok: true,
		stale: usdStale || fxFixingStale(e, b),
	}
}

// basisMinUSDVenues is how many distinct venues the USD leg needs before
// it may displace a single-venue fiat book (ADR-0053).
const basisMinUSDVenues = 2

// preferUSDAnchoredBasis is the ADR-0053 basis rule for a directly read
// non-fiat/fiat snapshot: a single-venue book yields to the USD-anchored
// derivation when that derivation is fresh and rests on a multi-venue USD
// leg. ok=false keeps the direct market — including every case where the
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

// convertAtFixing composes a USD price with a bound fixing in exact
// rationals (invariant 1) and stamps the fixing on the wire. observed_at
// stays the USD leg's: the fixing's own time is fx_as_of.
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

// closedFiatCross is [Server.tryFiatCrossRate] for the closed surfaces:
// both legs bind to fixings at the current minute, so every region serves
// the same answer until a bound bar changes.
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

// tryUSDAnchoredFiatCross prices a NON-fiat asset in any fiat we carry an FX rate for:
// price(asset, CCY) = price(asset, USD) x rate_usd[CCY] (ADR-0051). [tryFiatCrossRate] needs BOTH
// sides fiat. This is the tip's arm; closed surfaces use [Server.closedUSDAnchoredFiatCross].
//
// ok=false when: asset is fiat; quote is not fiat or is fiat:USD (crossing would loop); the
// currencies reader isn't wired or warmed; the rate is missing or non-positive; the FX leg is
// older than [Server.fxCrossMaxAge], so a wedged forex worker can't keep serving ever-older
// prices; or the asset has no USD price.
//
// The second return is `withheld`, and it is load-bearing: every withholding decision (scam
// issuer, decimals guard) is made on the USD leg, so multiplying an FX rate onto a withheld leg
// would publish the declined price through an ungated side door. It propagates and serves NOTHING.
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
	// fxObservedAt is the FX leg's own freshness: the older of the snapshot's publication time and
	// this ticker's UpdatedAt (the held-rate rule of [tryFiatCrossRate]). [Cache.Latest] never
	// expires on its own, so without this bound an fx worker outage would serve the last fetch forever.
	fxObservedAt := olderNonZero(fx.PublishedAt, rateUpdatedAt)
	if fxCrossStale(fxObservedAt, s.fxCrossMaxAge) {
		return PriceSnapshot{}, nil, false, false
	}

	usdSnap, usdSources, usdServed, _, ok, withheld := s.resolveUSDLeg(ctx, asset)
	if withheld {
		return PriceSnapshot{}, nil, false, true
	}
	// A declared peg is 1:1 against USD only; multiplying it by a rate is
	// neither the declaration nor an observation, so no cross is served.
	if !ok || isDeclaredPeg(usdSnap) {
		return PriceSnapshot{}, nil, false, false
	}

	// Exact rational arithmetic on a served price (ADR-0003). The float64 feed rate is converted
	// via its shortest round-trip decimal, so the product adds no rounding beyond the source
	// rate's own precision (as [tryFiatCrossRate] does).
	usdRat, okU := new(big.Rat).SetString(usdSnap.Price)
	rateRat, okR := new(big.Rat).SetString(strconv.FormatFloat(rate, 'f', -1, 64))
	if !okU || !okR {
		return PriceSnapshot{}, nil, false, false
	}

	out := usdSnap
	out.AssetID = asset.String()
	out.Quote = quote.String()
	out.Price = formatCrossRate(new(big.Rat).Mul(usdRat, rateRat))
	// observed_at is the OLDER of the two legs (same rule as
	// [tryFiatCrossRate]): the USD leg's own market observation, or the
	// FX rate's own freshness, whichever is coarser. Stamping the fresh
	// USD leg's timestamp alone would hide a stale FX component behind
	// an observed_at that looked current.
	out.ObservedAt = WireTime(olderNonZero(time.Time(usdSnap.ObservedAt), fxObservedAt))
	out.usdLeg = &usdLegFacts{sources: usdSources, served: usdServed}
	return out, appendFXSource(usdSources, rateSource), true, false
}

// defaultFXCrossMaxAge is the staleness budget for [Server.fxCrossMaxAge] when config leaves it
// unset. It mirrors [orchestrator.DefaultCompositeReferenceFXMaxAge] (76h): same fx_quotes source,
// daily buckets that pause over market closes. Duplicated to avoid a cross-binary import for one number.
const defaultFXCrossMaxAge = 76 * time.Hour

// fxCrossMaxAgeOrDefault resolves the configured
// pricing_guard.fx_cross_max_age_hours to a duration, falling back to
// [defaultFXCrossMaxAge] when unset. A negative value never reaches
// here — config.Validate rejects it at load.
func fxCrossMaxAgeOrDefault(hours int) time.Duration {
	if hours <= 0 {
		return defaultFXCrossMaxAge
	}
	return time.Duration(hours) * time.Hour
}

// fxCrossStale reports whether an FX leg observed at observedAt is too
// old to serve, per the staleness gate shared by
// [Server.tryFiatCrossRate] and [Server.tryUSDAnchoredFiatCross]. A
// zero observedAt (no per-currency or snapshot timestamp at all) is
// unknown freshness, not proven-fresh, and is treated as stale.
func fxCrossStale(observedAt time.Time, maxAge time.Duration) bool {
	if observedAt.IsZero() {
		return true
	}
	return time.Since(observedAt) > maxAge
}

// resolveUSDLeg resolves asset/fiat:USD through the same layers /v1/price uses, so the derived
// local-currency price rests on exactly the number the USD endpoint would serve.
//
// It deliberately does NOT re-enter [Server.priceFallback], which would recurse into
// [tryUSDAnchoredFiatCross]; the Redis VWAP and stablecoin-proxy layers are the only ones that can
// produce a USD price for a non-fiat asset.
//
// Returns (ok, withheld): a withheld USD leg must neither be served nor reported as absent.
// served is the alias the closed bucket was read under, zero when a fallback layer answered.
// It reads no freeze marker (the tip is freeze-agnostic); freeze-aware surfaces apply it through
// [Server.resolveFrozenServeFor].
func (s *Server) resolveUSDLeg(
	ctx context.Context, asset canonical.Asset,
) (snap PriceSnapshot, sources []string, served canonical.Asset, stale, ok, withheld bool) {
	usd := defaultPriceQuote
	snap, sources, stale, served, err := s.readPriceWithAliasesServed(ctx, s.Prices, asset, usd)
	switch {
	case errors.Is(err, ErrPriceWithheld):
		return PriceSnapshot{}, nil, canonical.Asset{}, false, false, true
	case err == nil:
		return snap, sources, served, stale, true, false
	case !errors.Is(err, ErrPriceNotFound):
		return PriceSnapshot{}, nil, canonical.Asset{}, false, false, false
	}
	// A fallback USD leg is below the closed-bucket contract: stale.
	if snap, srcs, _, _, ok := s.tryRedisVWAPFallback(ctx, asset, usd); ok {
		return snap, srcs, canonical.Asset{}, true, true, false
	}
	snap, srcs, ok, withheld := s.tryStablecoinFiatProxy(ctx, asset, usd)
	if ok {
		return snap, srcs, canonical.Asset{}, true, true, false
	}
	return PriceSnapshot{}, nil, canonical.Asset{}, false, false, withheld
}

// primaryFXSource is the forex worker's primary feed, credited for an
// entry that carries no Source of its own.
const primaryFXSource = "massive"

// fxSourceOf names the feed that published c's rate, so a price
// converted through the standby credits the standby rather than the
// primary it replaced.
func fxSourceOf(c CurrencyEntry) string {
	if c.Source == "" {
		return primaryFXSource
	}
	return c.Source
}

// appendFXSource adds the FX feed to the USD leg's own sources without
// dropping them: a customer auditing a BRL price needs to see both the
// venues that set the USD price AND the feed that converted it.
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

// proxyPairGate is OPTIONALLY implemented by the wired [PriceReader] so
// [Server.tryStablecoinFiatProxy] can cheaply skip empty proxy pairs. storePriceReader does it with
// a bounded both-directions recent-existence probe over prices_1m; readers without it give every
// peg a full LatestPrice. See timescale.Store.RecentClosedVWAP1mExists.
type proxyPairGate interface {
	// RecentClosedVWAP1mExists reports whether base/quote has a closed
	// 1-minute VWAP bucket (either stored direction) inside the price
	// surface's freshness horizon. A false return means "no live proxy
	// pair here — skip the peg"; an error means "couldn't tell — let the
	// caller try LatestPrice anyway".
	RecentClosedVWAP1mExists(ctx context.Context, base, quote canonical.Asset) (bool, error)
}

// tryStablecoinFiatProxy rewrites X/fiat:USD to X/<classic USD peg> at read time, a safety net
// for deployments without [aggregate].enable_stablecoin_fiat_proxy: no on-chain trade quotes in
// fiat:USD, so the literal pair has no prices_1m rows.
//   - A non-peg asset walks [trades].usd_pegged_classic_assets in priority order
//     ([Server.walkUSDPegs]); the first non-stale asset/<peg> row wins.
//   - A declared peg ([Server.isDeclaredUSDPeg]) is priced through its XLM book
//     ([Server.crossDeclaredPegThroughXLM]). The declaration answers only when the cross has
//     nothing: a constant must not pre-empt an observation, since a depeg is when they disagree.
//     Pricing via another declared peg is not a route (a break there would invert the price).
//
// ok=false when the quote is not fiat:USD, no pegs are configured, or a non-peg asset finds no
// price. The snapshot is flagged triangulated. `withheld` (a peg leg returned ErrPriceWithheld)
// must not collapse into not-found, because the withheld body points at the raw surfaces; it is
// sticky but does not stop the walk. A refused declared-peg cross is withheld, not replaced by
// the declaration (a flat 1.0 would hide the refusal).
func (s *Server) tryStablecoinFiatProxy(ctx context.Context, asset, quote canonical.Asset) (PriceSnapshot, []string, bool, bool) {
	// Self-peg: a `crypto:<STABLE>` ticker priced in the fiat it tracks (crypto:USDC/fiat:USD,
	// crypto:EURC/fiat:EUR). The aggregator treats these tickers as that fiat
	// (aggregate.FiatProxy), so ~1.0 follows the same convention as every VWAP; a depeg surfaces
	// via the divergence subsystem, not here. The classic-issued form takes the declared-peg
	// route below. Runs ahead of the fiat:USD guard so it also serves the EUR/MXN pegs, and only
	// after the caller's literal read missed, so an observed bucket (the CEX tier quotes that
	// pair directly) still wins over the declaration.
	if proxy, isStable := aggregate.FiatProxy(asset); isStable && proxy.Equal(quote) {
		return s.declaredPegSnapshot(asset, quote), nil, true, false
	}
	// Everything returned before the walk carries withheld=false: no peg
	// leg has been read yet, so no withholding verdict can exist. The walk
	// is what makes that verdict sticky — see [Server.walkUSDPegs].
	if quote.Type != canonical.AssetFiat || quote.Code != "USD" {
		return PriceSnapshot{}, nil, false, false
	}
	if len(s.USDPeggedClassics) == 0 || s.Prices == nil {
		return PriceSnapshot{}, nil, false, false
	}
	if s.isDeclaredUSDPeg(asset) {
		snap, srcs, verdict := s.crossDeclaredPegThroughXLM(ctx, asset, quote)
		switch verdict {
		case pegXLMLegPriced:
			return snap, srcs, true, false
		case pegXLMLegRefused:
			return PriceSnapshot{}, nil, false, true
		case pegXLMLegNoMarket, pegXLMLegReadFailed, pegXLMLegFlagged:
		}
		// No market priced this peg and none was refused on substance, so publish the declaration
		// itself with nil sources: an empty set is not "single-sourced" (the handler's
		// len(sources)==1 rule leaves SingleSource=false), and flagging it would need a synthetic
		// entry in the wire `sources[]`.
		return s.declaredPegSnapshot(asset, quote), nil, true, false
	}
	snap, srcs, ok, withheld := s.walkUSDPegs(ctx, asset, quote)
	if ok {
		return snap, srcs, true, withheld
	}
	return PriceSnapshot{}, nil, false, withheld
}

// isDeclaredUSDPeg reports whether asset is one of the operator's
// [trades].usd_pegged_classic_assets under ANY of its spellings. A declared peg is an asset, not a
// spelling ([Server.usdPegProxyQuotes]): the SAC wrapping the classic id (`[supply].sac_wrappers`,
// folded by [sameAsset]) is the same asset. Matching the declared spelling alone sends the SAC
// down the sibling walk, whose only pair is itself against its classic form (not a market), so
// the two spellings would answer differently.
func (s *Server) isDeclaredUSDPeg(asset canonical.Asset) bool {
	for _, peg := range s.USDPeggedClassics {
		if sameAsset(peg, asset) {
			return true
		}
	}
	return false
}

// walkUSDPegs is [Server.tryStablecoinFiatProxy]'s allow-list walk: it tries asset/<peg> for each
// declared USD peg in priority order and returns the first observed price, re-quoted to fiat:USD.
//
// A declared peg never reaches this walk (the caller routes it through
// [Server.crossDeclaredPegThroughXLM] first), so each pair read here has a peg on exactly one
// side. Answering a flat $1 before reading any market would hide a depeg behind the declaration.
//
// withheld carries the sticky ErrPriceWithheld verdict described on
// [Server.tryStablecoinFiatProxy].
func (s *Server) walkUSDPegs(
	ctx context.Context, asset, quote canonical.Asset,
) (snapshot PriceSnapshot, sources []string, ok, withheld bool) {
	// Empty-proxy-pair gate. A <peg> quote is a CLASSIC asset, so on a VWAP miss LatestPrice
	// falls through to an UNBOUNDED last-trade scan; a pure-Soroban token that only trades vs XLM
	// has zero rows for every <token>/<peg> and would pay that full-history walk once PER PEG.
	// When the reader exposes the cheap bounded recent-existence probe, skip any peg with no
	// closed 1m bucket in the freshness horizon. A gate ERROR falls through to LatestPrice so a
	// probe blip can never suppress a real price.
	gate, _ := s.Prices.(proxyPairGate)
	for _, peg := range s.USDPeggedClassics {
		if gate != nil {
			exists, gerr := gate.RecentClosedVWAP1mExists(ctx, asset, peg)
			if gerr == nil && !exists {
				// No closed 1m bucket for asset/<peg> in the freshness
				// horizon — skip before the unbounded last-trade walk.
				continue
			}
			// gerr != nil: fall through to LatestPrice (best-effort — a
			// probe error must not hide a price the walk could still find).
		}
		snap, srcs, _, err := s.Prices.LatestPrice(ctx, asset, peg)
		if err != nil {
			// A WITHHELD verdict is not a miss — it means this asset HAS
			// a price on the peg leg that policy declines to publish.
			// Record it and keep going (a later peg may still serve, which
			// beats a 404); if none does, the caller reports withheld
			// rather than not-found.
			if errors.Is(err, ErrPriceWithheld) {
				withheld = true
			}
			// ErrPriceNotFound is the common case (peg not active for
			// this asset); any other error gets the same treatment as
			// the chart fallback — silent skip, try the next peg.
			continue
		}
		// Nonstandard-decimals forward normalization. LatestPrice returns the RAW asset/<peg>
		// ratio and `asset` may be a non-7-decimals Soroban leg (the <peg> is always 7dp). This is
		// the SINGLE place the proxy tier is corrected, so no caller re-normalizes. It resolves
		// against the ACTUAL traded legs (asset, peg), NOT the requested fiat:USD.
		s.normalizeRawPriceSnapshot(&snap, asset, peg)
		// Rewrite the snapshot's Quote field so the wire response
		// reflects what the user asked for, not the proxy peg.
		snap.Quote = quote.String()
		return snap, srcs, true, withheld
	}
	return PriceSnapshot{}, nil, false, withheld
}

// crossDeclaredPegThroughXLM prices a declared USD peg in fiat:USD through its XLM book:
// price(peg/fiat:USD) = price(peg/XLM) x price(XLM/fiat:USD).
//
// The point twin of [Server.fiatSeriesThroughXLM], sharing [crossThroughPivot]: exact big.Rat
// (ADR-0003), a zero, negative or missing leg is a miss. No venue quotes a classic peg in
// fiat:USD, so this is what makes a depeg visible instead of the flat $1 declaration. The pivot
// is read direct (XLM/fiat:USD), never via the Redis composite or stablecoin proxy: a pivot
// priced through the peg under test would cancel to 1.
//
// Cost: [Server.readDeclaredPegXLMLeg] tries up to six spelling x XLM-form combinations; each
// `native`- or SAC-quoted miss is an unbounded last-trade scan, so each sits behind a
// [proxyPairGate] probe (a probe error falls through to the read).
//
// A withheld leg stops the spelling walk: the scam gate can't fire on a SAC base, so moving to the
// SAC book would publish the market the gate declined. observed_at is the older leg's.
func (s *Server) crossDeclaredPegThroughXLM(
	ctx context.Context, asset, quote canonical.Asset,
) (PriceSnapshot, []string, pegXLMLegVerdict) {
	xlm := canonical.NativeAsset()
	if sameAsset(asset, xlm) {
		return PriceSnapshot{}, nil, pegXLMLegNoMarket
	}
	pegLeg, pegSources, verdict := s.readDeclaredPegXLMLeg(ctx, asset)
	if verdict != pegXLMLegPriced {
		return PriceSnapshot{}, nil, verdict
	}
	xlmLeg, xlmSources, _, err := s.readPriceWithAliases(ctx, s.Prices, xlm, quote)
	switch {
	case errors.Is(err, ErrPriceWithheld):
		return PriceSnapshot{}, nil, pegXLMLegRefused
	case errors.Is(err, ErrPriceNotFound):
		return PriceSnapshot{}, nil, pegXLMLegNoMarket
	case err != nil:
		return PriceSnapshot{}, nil, pegXLMLegReadFailed
	}
	// Both legs are RAW ratios off the reader; each is decimals-corrected
	// (M2) against the legs it actually traded before the product is
	// taken. Byte-identical no-op for 7dp legs.
	s.normalizeRawPriceSnapshot(&xlmLeg, xlm, quote)
	price, ok := crossThroughPivot(pegLeg.Price, xlmLeg.Price)
	if !ok {
		return PriceSnapshot{}, nil, pegXLMLegNoMarket
	}
	observedAt := pegLeg.ObservedAt
	if xlmLeg.ObservedAt.Time().Before(observedAt.Time()) {
		observedAt = xlmLeg.ObservedAt
	}
	// The product spans the wider of the two windows. A leg served off
	// the reader's last-trade tail carries none, so the other leg's
	// window governs.
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
	}, unionSources(pegSources, xlmSources), pegXLMLegPriced
}

// readDeclaredPegXLMLeg reads asset/XLM for [Server.crossDeclaredPegThroughXLM]: each peg
// spelling as base against each XLM alias as quote. The walk advances only on pegXLMLegNoMarket.
//
//   - pegXLMLegPriced ends it. Fresh beats stale only within a spelling, so a dormant SDEX book
//     outranks a fresh thin Soroban pool (as [tipMergePairs]). A zero, negative or unparsable
//     price also counts as answered: one degenerate print on the deep book must not hand it to a
//     thin pool.
//   - pegXLMLegRefused / pegXLMLegFlagged end it: the scam gate never fires on a non-classic
//     base, so advancing to the SAC book would republish the refused market.
//   - pegXLMLegReadFailed ends it and the declaration answers: a broken read is not evidence of
//     no classic book.
//
// Order is canonical-first ([canonical.CanonicalAsset], SAC last), not the literal-first order of
// [Server.readPriceWithAliases], so the classic id and the C-address price one asset from one
// book. This departs from [canonical.AssetAliases]' contract; the residual is accepted in §7 of
// the thin-pool third-alias VWAP review (docs/methodology/).
func (s *Server) readDeclaredPegXLMLeg(
	ctx context.Context, asset canonical.Asset,
) (PriceSnapshot, []string, pegXLMLegVerdict) {
	xlmForms := assetAliases(canonical.NativeAsset())
	for _, base := range assetAliases(canonical.CanonicalAsset(asset)) {
		snap, srcs, verdict := s.readPegXLMLegForSpelling(ctx, base, xlmForms)
		if verdict != pegXLMLegNoMarket {
			// Priced, refused or failed — this spelling settles the
			// leg. Only "found nothing" reaches the next spelling.
			return snap, srcs, verdict
		}
	}
	return PriceSnapshot{}, nil, pegXLMLegNoMarket
}

// pegXLMLegVerdict is what one read on the declared peg's XLM leg SAID, as distinct from whether it
// yielded a price: the spelling walk advances on one value and stops on the others, so collapsing
// them into ok=false would turn a refusal or broken read into "no market, try the next spelling"
// and ultimately into the declaration.
//
// Values ascend by how much a read SAYS, so a spelling's verdict is the maximum of its
// combinations', the precedence [Server.readPriceWithAliases] gives a withheld verdict over
// not-found.
type pegXLMLegVerdict int

const (
	// pegXLMLegNoMarket — the [proxyPairGate] probe reported no recent
	// closed bucket, or the read reported ErrPriceNotFound. The ONLY
	// verdict that lets the walk move to the peg's next spelling.
	pegXLMLegNoMarket pegXLMLegVerdict = iota
	// pegXLMLegReadFailed — the read returned an error that is neither
	// not-found nor withheld: transport, query planning, timeout. Says
	// nothing about whether a market exists, so the walk stops.
	pegXLMLegReadFailed
	// pegXLMLegFlagged — the read returned ErrPriceWithheld for a
	// directory-flagged issuer. The walk stops, and the declaration still
	// answers: the scam gate keys on the classic issuer, so its verdict is
	// what stops a SAC spelling republishing that issuer's refused market,
	// and the declaration is what the route served before that spelling
	// was walked at all.
	pegXLMLegFlagged
	// pegXLMLegRefused — the read returned any other ErrPriceWithheld:
	// the substance gate (measured on the alias union, so it is the
	// asset's market that is thin), or a refusal that did not say why.
	// The walk stops (see [Server.readDeclaredPegXLMLeg]) and
	// the route withholds rather than print the declaration over it.
	// Ranked above pegXLMLegFlagged so a mix fails closed.
	pegXLMLegRefused
	// pegXLMLegPriced — a snapshot was read. Freshness is carried
	// separately; a stale snapshot is still priced.
	pegXLMLegPriced
)

// readPegXLMLegForSpelling reads ONE spelling of the peg against every XLM form for
// [Server.readDeclaredPegXLMLeg]. pegXLMLegNoMarket means every XLM form gate-missed or read
// not-found, the one condition under which the caller moves to the next spelling.
//
// Among answering forms a fresh one wins over a stale one, and a stale one still serves when it is
// all this spelling has (as [Server.readPriceWithAliases]). Alias order alone would take `native`
// however old over a fresh SAC-quoted book: XLM's forms are disjoint venue populations and the
// book that traded last prices the peg now. The preference is confined to one spelling; across
// spellings it would rank a thin pool over a dormant deep book.
//
// A refusal or read failure on one XLM form does NOT stop this spelling, since a later form may
// still price the peg; only when none priced does it decide the verdict.
func (s *Server) readPegXLMLegForSpelling(
	ctx context.Context, base canonical.Asset, xlmForms []canonical.Asset,
) (PriceSnapshot, []string, pegXLMLegVerdict) {
	var staleSnap PriceSnapshot
	var staleSources []string
	staleFound := false
	spelling := pegXLMLegNoMarket
	for _, xlm := range xlmForms {
		snap, srcs, stale, verdict := s.readPegXLMLegPair(ctx, base, xlm)
		if verdict != pegXLMLegPriced {
			// Keep the loudest thing any form said, so a refusal or a
			// failure outranks a plain not-found (see the type).
			spelling = max(spelling, verdict)
			continue
		}
		if !stale {
			return snap, srcs, pegXLMLegPriced
		}
		// Stale — keep the first one in case no form answers fresh.
		if !staleFound {
			staleSnap, staleSources, staleFound = snap, srcs, true
		}
	}
	if staleFound {
		return staleSnap, staleSources, pegXLMLegPriced
	}
	return PriceSnapshot{}, nil, spelling
}

// readPegXLMLegPair is one (peg spelling, XLM form) combination of [Server.readDeclaredPegXLMLeg]'s
// walk. It reports which of five [pegXLMLegVerdict] outcomes was reached, a distinction the
// spelling walk turns on and so must not collapse into a bare miss:
//
//   - pegXLMLegNoMarket: the [proxyPairGate] probe saw no recent closed bucket, or LatestPrice
//     returned ErrPriceNotFound.
//   - pegXLMLegFlagged: ErrPriceWithheld for a flagged issuer ([PriceWithheldScamIssuer]).
//   - pegXLMLegRefused: any other ErrPriceWithheld.
//   - pegXLMLegReadFailed: any other LatestPrice error.
//   - pegXLMLegPriced: a snapshot, with `stale` alongside it.
//
// A gate ERROR falls through to the read, so a probe blip can never hide a price. The snapshot is
// decimals-normalized against the legs actually traded.
func (s *Server) readPegXLMLegPair(
	ctx context.Context, base, xlm canonical.Asset,
) (PriceSnapshot, []string, bool, pegXLMLegVerdict) {
	if gate, wired := s.Prices.(proxyPairGate); wired {
		if exists, gerr := gate.RecentClosedVWAP1mExists(ctx, base, xlm); gerr == nil && !exists {
			return PriceSnapshot{}, nil, false, pegXLMLegNoMarket
		}
	}
	snap, srcs, stale, err := s.Prices.LatestPrice(ctx, base, xlm)
	switch {
	case errors.Is(err, ErrPriceWithheld) && priceWithheldReason(err) == PriceWithheldScamIssuer:
		return PriceSnapshot{}, nil, false, pegXLMLegFlagged
	case errors.Is(err, ErrPriceWithheld):
		return PriceSnapshot{}, nil, false, pegXLMLegRefused
	case errors.Is(err, ErrPriceNotFound):
		return PriceSnapshot{}, nil, false, pegXLMLegNoMarket
	case err != nil:
		return PriceSnapshot{}, nil, false, pegXLMLegReadFailed
	}
	s.normalizeRawPriceSnapshot(&snap, base, xlm)
	return snap, srcs, stale, pegXLMLegPriced
}

// unionSources merges two legs' source sets — deduplicated, sorted — the
// way [appendFXSource] credits the FX feed beside the USD leg's venues:
// a consumer auditing a crossed price needs every venue behind it.
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

// declaredPegPrice is the operator's standing 1:1 declaration, rendered
// at the 12 fractional digits this surface publishes for it — the bytes
// are part of the contract, so they stay put.
const declaredPegPrice = "1.000000000000"

// declaredPegSnapshot renders the operator's peg declaration as a snapshot: what /v1/price serves
// for a declared peg when NO market observation exists (direct read missed and
// [Server.crossDeclaredPegThroughXLM] found no XLM book), and for a `crypto:<STABLE>` ticker in
// its own fiat.
//
// ObservedAt is the declaration's adoption time ([Server.pegDeclaredAt]), never time.Now(): the
// constant is not an observation, and a clock stamp would make it indistinguishable from one on
// the wire. The adoption instant lets the answer AGE, so staleness reads "nothing observed since
// the declaration was adopted", which is true.
//
// The stamp is per process (the construction time unless [Options.PegDeclaredAt] is set) and
// resets on restart; price_type "peg" is the durable signal that nothing was observed.
func (s *Server) declaredPegSnapshot(asset, quote canonical.Asset) PriceSnapshot {
	return PriceSnapshot{
		AssetID:    asset.String(),
		Quote:      quote.String(),
		Price:      declaredPegPrice,
		PriceType:  "peg",
		ObservedAt: WireTime(s.pegDeclaredAt),
	}
}

// tryFiatCrossRate synthesises a fiat-vs-fiat price through the CurrenciesReader's USD-base
// snapshot; the last-resort /v1/price fallback (fiat pairs have no on-chain trades).
//
// rate_usd[X] = "1 USD = N units of X", so 1 X in Y = rate_usd[Y] / rate_usd[X]; (fiat:X,
// fiat:USD) is special-cased to 1/rate_usd[X].
//
// ok=false when either side isn't fiat, the reader isn't wired or warmed, a ticker is missing, or
// rate_usd[X] is zero. PriceType is "vwap" because the forex feed is itself volume-weighted.
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

	// Build a lookup from ticker → rate_usd. The slice is small
	// (~110 entries) and called rarely (last-resort fallback), so
	// linear scan is fine; the alternative — a per-request map
	// allocation — would not pay off.
	var rateAsset, rateQuote float64
	var foundAsset, foundQuote bool
	var sources []string
	// observedAt starts at the snapshot's publication time and is pulled
	// BACK to the older leg's own UpdatedAt. The two normally coincide;
	// they differ exactly when the forex worker is HOLDING a leg's last
	// guarded rate because the sanity band refused the upstream's new
	// one (or the standby feed does not carry the currency). Stamping a
	// held rate with the fresh publication time would claim a freshness
	// it does not have. Older-leg-wins is the rule the XLM-pivot peg
	// cross in this file already applies to its two legs.
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
	// rate_usd[USD] is implicitly 1 — handle that without requiring
	// the snapshot to carry an explicit USD entry. foundQuote is
	// not read after this branch; the implicit-USD path just
	// supplies rateQuote.
	if !foundQuote {
		if quote.Code != "USD" {
			return PriceSnapshot{}, nil, false
		}
		rateQuote = 1
	}
	// [Cache.Latest] never expires on its own, so without this bound a forex worker outage would
	// serve the last fetch forever. observedAt is the older leg's, so one check covers both a dead
	// worker (stale PublishedAt) and a held ticker past its bound (stale per-leg UpdatedAt).
	if fxCrossStale(observedAt, s.fxCrossMaxAge) {
		return PriceSnapshot{}, nil, false
	}
	// Cross-rate = rate_usd[Y] / rate_usd[X] as exact big.Rat, not float64 (ADR-0003). Each float64
	// feed rate is converted via its shortest round-trip decimal, so the division adds no rounding
	// beyond the source rates' own precision.
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

// olderNonZero returns the earlier of a and b, ignoring a zero b (a
// snapshot entry that carries no per-currency timestamp).
func olderNonZero(a, b time.Time) time.Time {
	if !b.IsZero() && b.Before(a) {
		return b
	}
	return a
}

// formatCrossRate serialises a fiat cross-rate big.Rat to a decimal
// string with up to 15 fractional digits, trailing zeros trimmed. 15
// places comfortably covers the precision an ECB-class FX rate carries
// while keeping the exact-rational computation off the wire as a
// bounded decimal (a non-terminating ratio like 1/3 would otherwise
// have no finite form). A rate too small for 15 places gets more
// ([priceRenderScale]) so a positive cross-rate never serves as "0".
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

// attachConfidence consults the wired ConfidenceLooker (when set)
// and populates snap.Confidence + snap.ConfidenceFactors. Best-
// effort: cache misses + read errors leave the fields nil so the
// response still ships cleanly without confidence enrichment.
func (s *Server) attachConfidence(r *http.Request, snap *PriceSnapshot, asset, quote canonical.Asset, window time.Duration) {
	if s.Confidence == nil {
		return
	}
	// Loop the alias set as readPriceWithAliases does for the price: the price read resolves
	// `native` to crypto:XLM, but a confidence key built from the raw request asset finds
	// nothing, and clients read an absent confidence as "unknown".
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

// lookupDivergenceFlag is the shared best-effort divergence lookup for every surface serving a
// value the cross-reference verdict speaks to (/v1/price, its windowed variant, /v1/price/tip and
// its stream, per event from the producer goroutine, hence a context). Returns (false, false) when
// no looker is wired, there is no verdict, it does not speak to the served window, or the lookup
// errors: better a fresh price without a verdict than a 5xx on a Redis blip.
//
// asset and quote are the spelling the value was SERVED from, and only that pair is asked: verdicts
// are keyed on the literal pair priced and XLM's spellings are disjoint venue populations, so
// walking aliases would attach crypto:XLM's CEX-checked verdict to native's SDEX price.
//
// window is the served value's aggregation window, or 0 for the current price; a verdict over
// another window says nothing about a windowed value. A context-caused error is not logged; the
// test is on the ERROR, not ctx.Err(), so a genuine store failure is still logged.
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

// attachCompositeFlags surfaces the router-quality signals flags.diverged and flags.rerouted
// persisted at [cachekeys.VWAPCompositeMeta] for every router-priced target.
//
// requireTriangulated controls whether flags.Triangulated must already be true to look up:
//   - The ?window= surface reads one per-(pair,window) key owned by the composite or a direct
//     writer; when the direct writer served it, meta from a DIFFERENT window/tick does not
//     describe the value. Pass true.
//   - The headline surface reads prices_1m FIRST, so a router target with a real closed bucket
//     never sets flags.Triangulated though its chain ran and wrote meta. Pass false.
//
// Gated on the looker also implementing the OPTIONAL [CompositeMetaLooker]. Best-effort: a miss,
// malformed blob or read error leaves the flags unset. pair and window must be the ones the
// served value was read under.
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
		// A malformed blob is enrichment noise, not a serving failure —
		// the price is already computed. Log and leave the flags unset.
		s.logger.Warn("composite meta decode failed",
			"err", err, "asset", asset.String(), "quote", quote.String())
		return
	}
	flags.Diverged = meta.Diverged
	flags.Rerouted = meta.Rerouted
	flags.PivotUnverified = meta.PivotUnverified
}

// lookupFrozen consults the FrozenLooker (when wired) for the
// supplied pair and returns whether the most-recent published bucket
// was frozen. checked is false when no verdict was reached (looker
// not wired, or the marker read failed) — same two-valued posture as
// lookupDivergenceFlag, so a caller can tell "confirmed not frozen"
// from "unknown" instead of silently reading the latter as the former.
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

// frozenServe is the outcome of [Server.resolveFrozenServe]. Three
// states, spelled out: "frozen but nothing to serve" is its own state
// and must never travel as one of the other two — read as "not frozen"
// it publishes the bucket the freeze is withholding; read as "held" it
// publishes an empty price.
type frozenServe int

const (
	// frozenServeNotFrozen — no live freeze; serve what was read.
	frozenServeNotFrozen frozenServe = iota
	// frozenServeHeld — frozen, and resolution.snapshot is the held value.
	frozenServeHeld
	// frozenServeNothingHeld — frozen, and no held value is readable.
	// The caller serves NOTHING for the pair.
	frozenServeNothingHeld
)

// frozenResolution carries [Server.resolveFrozenServe]'s verdict; snapshot, sources and
// triangulated are meaningful only for frozenServeHeld. checked=false means the marker could not
// be read (looker unwired or read failed): outcome is then frozenServeNotFrozen by construction but
// means "unknown", so a caller must not derive other facts (e.g. single_source) from it.
type frozenResolution struct {
	outcome      frozenServe
	checked      bool
	snapshot     PriceSnapshot
	sources      []string
	triangulated bool
	// pairBase is the spelling whose marker fired and whose held value was
	// read; the composite-meta lookup must ask for the same pair.
	pairBase canonical.Asset
}

// frozenHeldWindows are the aggregator windows a freeze can hold a value for
// ([orchestrator.DefaultWindows]), smallest first. Freeze runs per (pair, window) but the marker
// is per pair (ADR-0019), so it can't say which window holds the value; smallest-first stays
// closest to the 1-minute bucket and matches [triangulationLookupWindow].
var frozenHeldWindows = []time.Duration{5 * time.Minute, time.Hour, 24 * time.Hour}

// resolveFrozenServe makes `flags.frozen` true in the only honest way: by making the VALUE the one
// the freeze is holding (ADR-0019).
//
// The closed-bucket read can't deliver the last-known-good value: prices_1m is a raw
// sum(quote)/sum(base) the anomaly checker never gates, so its newest bucket on a frozen pair is
// the very bucket the freeze refused, and stamping `frozen` on it would mislead. What a freeze
// holds lives in the aggregator's VWAP cache, kept alive while the window skips its publish
// (keepFrozenVWAPAlive).
//
// `served` is the alias whose market the closed-bucket read came from (zero on a miss). Markers
// are keyed on the literal pair the aggregator prices, so a `native` request answered from
// crypto:XLM's bucket is governed by crypto:XLM's freeze only (see [Server.frozenPairBase]).
//
// No readable held value (first-bucket freeze, expired key, cache error) is
// frozenServeNothingHeld and the caller refuses: the only other value is the one withheld.
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
				// When the held value was observed, not when it was read:
				// the hold is what makes those differ by tens of minutes.
				ObservedAt:    WireTime(v.ObservedAt),
				WindowSeconds: int(window / time.Second),
			}, v.Coverage),
		}
	}
	return frozenResolution{outcome: frozenServeNothingHeld, checked: checked}
}

// resolveFrozenServeFor is [Server.resolveFrozenServe] for the snapshot
// in hand. An ADR-0051 cross is governed by its USD leg's marker, read
// exactly as /v1/price?quote=fiat:USD reads it, never by the derived
// fiat pair's (no aggregator writes one). A frozen leg serves its held
// value converted at a fixing, or nothing when none is held or none binds —
// never the refused bucket times the rate.
func (s *Server) resolveFrozenServeFor(r *http.Request, snap PriceSnapshot, requested, served, quote canonical.Asset) frozenResolution {
	leg := snap.usdLeg
	if leg == nil {
		return s.resolveFrozenServe(r, requested, served, quote)
	}
	held := s.resolveFrozenServe(r, requested, leg.served, defaultPriceQuote)
	if held.outcome != frozenServeHeld {
		return held
	}
	// The held value converts at the fixing bound to its own bucket end, so
	// the hold serves one answer however long it lasts.
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

// frozenPairBase reports which spelling of the pair carries the freeze marker that GOVERNS this
// response: one marker read when a bucket was served, one per spelling of the requested asset
// when none was.
//
// When a closed bucket was read, only the marker of the alias it was read from counts: the
// requested literal's marker is a verdict on a DIFFERENT venue population (`native/fiat:GBP` is
// not the market `crypto:XLM/fiat:GBP` is), and letting it fire would discard a healthy bucket.
// A healthy alias wins, as [Server.readPriceWithAliasesServed] does for a withheld alias.
//
// When no bucket was read (`served` is zero, the fallback chain answered) every spelling's marker
// governs: the chain resolves across the alias set without reporting which leg it served, so a
// freeze on `crypto:XLM/fiat:GBP` must not go unseen for `native/fiat:GBP`. See
// [Server.frozenAnyAlias].
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

// frozenAnyAlias reads the freeze marker of every spelling of requested
// against quote. A frozen spelling governs at once. Otherwise checked
// is true only when every marker was read: one failed read leaves that
// spelling's verdict unknown, and "unknown" must not travel as
// "confirmed not frozen".
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

// writeFrozenNothingHeldProblem is the single-asset refusal for frozenServeNothingHeld. It is a 503,
// not a 404, because prices exist and the freeze clears on its own, so retry is right. It has its
// own title and detail, since the price-withheld "market too thin" wording would misstate why.
func writeFrozenNothingHeldProblem(w http.ResponseWriter, r *http.Request, asset, quote canonical.Asset) {
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/price-unavailable",
		"Price frozen — no last-known-good value available", http.StatusServiceUnavailable,
		asset.String()+" / "+quote.String()+
			" is frozen by anomaly detection (ADR-0019) and no last-known-good value is currently held,"+
			" so the refused bucket is not published in its place — retry shortly,"+
			" or read the raw market via /v1/price/tip or /v1/observations and apply your own judgement")
}

// holdFrozenBatchRow is [Server.resolveFrozenServe] for one
// /v1/price/batch row: a frozen row carries the held value, and a
// frozen row with nothing held is omitted — the batch contract's only
// spelling of "no price for this asset" (it has no per-row problem
// shape; callers probe /v1/price for the reason).
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

// handlePriceBatch serves GET /v1/price/batch?asset_ids=A,B,C&quote=<id>.
//
// Missing observations are omitted, not 404'd: "prices for some assets but not others" differs
// from "malformed request", and a caller sees which assets are missing from the rows returned.
//
// Limits: asset_ids count 1..100 (priceBatchMaxAssets; POST accepts up to 1000 in the body);
// duplicates are de-duplicated server-side. The envelope Stale flag is the OR over per-row stale
// flags, matching single-asset /v1/price.
func (s *Server) handlePriceBatch(w http.ResponseWriter, r *http.Request) {
	// `pairs=` is accepted as an alias for
	// `asset_ids=` so clients arriving via cross-endpoint
	// extrapolation (CG-style sites that call a quotes endpoint
	// "pairs") don't hit a confusing 400 on their first try.
	// Passing both is a 400 rather than a silent precedence.
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

// handlePriceBatchPost serves POST /v1/price/batch with JSON body {"asset_ids": [...], "quote":
// "..."}. Same semantics as GET with the ceiling raised to 1000; the body exists because a
// 1000-entry query string exceeds most reverse-proxies' default 8 KiB header limit.
func (s *Server) handlePriceBatchPost(w http.ResponseWriter, r *http.Request) {
	// Cap the request body so a malicious client can't make us spend
	// memory parsing a 100 MiB JSON object. 1 MiB is plenty for 1000
	// canonical asset ids — the largest realistic ones (contract
	// strkeys at 56 bytes + quotes/commas) are well under 100 KiB.
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

// runPriceBatch is the shared core of GET + POST /v1/price/batch: it trims, de-duplicates,
// enforces `limit`, parses the quote (default fiat:USD) and writes the response, or has already
// written a problem+json. `rawIDs` arrive in the user's order and output keeps first-occurrence
// order. Each helper writes its own problem+json on failure and signals via a sentinel return.
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
	// The monthly meter counts the same de-duplicated ids: one request
	// unit per price looked up, as a GET /v1/price per id would cost.
	// After the rate-limit charge, so a 429 above meters one unit.
	middleware.ChargeUsage(r, len(ids))
	s.lookupPriceBatch(w, r, ids, quote)
}

// parsePriceBatchIDs trims, de-duplicates, and length-checks the
// requested asset_ids. ok=false means a problem+json has already
// been written.
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

// parsePriceBatchQuote parses the optional quote, defaulting to
// fiat:USD. ok=false means a 400 has already been written.
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

// priceBatchConcurrency bounds the per-request fan-out in lookupPriceBatch. Each of up to 100 (GET)
// / 1000 (POST) ids is a LatestPrice + fallback-chain DB round-trip; serial resolution put p99 at
// the 10s handler ceiling.
//
// The value is MEASURED; do not lower it as a throughput control. Narrowing removes no database
// work, only lengthens how long each request holds its connections.
//
// 16-wide can exceed the DB pool's headroom for a 1000-id POST (~2 round-trips per id). The real
// bound there is the rate limiter, charged PER ID (runPriceBatch via middleware.ChargeRateLimit).
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
	// withheld qualifies a skip: a serving gate declined to publish a
	// price the server holds, so the id goes on the envelope's
	// `withheld` list rather than reading as "no data".
	withheld bool
	// withheldReason is the gate's reason behind withheld, the key of the
	// include_thin second pass.
	withheldReason PriceWithheldReason
	// thin: served under include_thin from a market below the substance
	// floor; such a row carries no change_24h_pct.
	thin bool
	// readFailed qualifies a skip: the row was dropped because a read
	// failed, not because the asset has no price.
	readFailed bool
	fail       *batchRowFailure
}

// batchRowFailure means the whole batch must abort with a
// problem+json. It is carried as a value rather than written
// inline because resolveBatchRow runs in a worker goroutine, where
// touching the shared http.ResponseWriter would be a data race —
// the orchestrator writes the first failure single-threaded.
type batchRowFailure struct {
	status int
	typ    string
	title  string
	detail string
}

// resolveBatchRow resolves one (raw_id, quote) pair for the batch endpoint. It is goroutine-safe
// (reads only, never touches w) and returns ok (a row), skip (per-asset miss, omitted) or fail (a
// validation/internal error aborting the batch).
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
	// No derived change from a thin price (ADR-0018). Computed on the
	// final snapshot, so a frozen row's change is the held value's.
	if row.ok && !row.thin {
		row.snap.Change24hPct = s.batchChange24h(ctx, asset, quote, row.snap.Price)
	}
	return row
}

// readBatchRow is one pass of a batch row's read under the
// [ThinAdmission] ctx carries.
func (s *Server) readBatchRow(ctx context.Context, r *http.Request, asset, quote canonical.Asset) batchRowResult {
	// Route the primary read through the XLM dual-form alias loop,
	// exactly as handlePrice does. Querying the literal form only would
	// make asset_ids=native return stale/empty while
	// /v1/price?asset=native serves fresh CEX VWAP published under the
	// crypto:XLM alias key.
	snap, sources, stale, served, err := s.readPriceWithAliasesServed(ctx, s.Prices, asset, quote)
	if errors.Is(err, ErrPriceWithheld) {
		// Gated pair: omit the row and name it on the envelope's
		// `withheld` list, and do NOT run priceFallback — the fallback
		// chain would re-serve the withheld market via the Redis/proxy
		// side doors.
		return batchRowResult{skip: true, withheld: true, withheldReason: priceWithheldReason(err)}
	}
	if errors.Is(err, ErrPriceNotFound) {
		// Share /v1/price's full fallback chain (priceFallback): skipping the stablecoin proxy would
		// silently drop ids that return 200 on /v1/price. A withheld verdict from any leg goes on
		// the envelope's `withheld` list, never folded into "no data".
		fb := s.priceFallback(ctx, asset, quote)
		fs, fsrc, fserved, ftri := fb.snap, fb.sources, fb.served, fb.triangulated
		if fb.ok {
			// Fallback responses are below the closed-bucket VWAP contract, so mark stale. Carry
			// triangulated through too, for parity with single-asset /v1/price. fserved is the
			// alias the cached VWAP was read under (zero when a later layer answered), the
			// governing pair for the freeze check.
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
			// Client disconnected — the orchestrator detects the
			// cancelled context after join and returns without
			// writing. Surface as a skip so no failure is reported.
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
	// Nonstandard-decimals normalization: this is the raw closed-1m-bucket read, the same raw-ratio
	// shape as /v1/price's primary path. The priceFallback branch above is already normalized and
	// must NOT be corrected twice; see normalizeRawPriceSnapshot.
	s.normalizeRawPriceSnapshot(&snap, asset, quote)
	// A frozen row carries the value the freeze is holding, not
	// the raw bucket just read — see [Server.resolveFrozenServe].
	return s.holdFrozenBatchRow(r, batchRowResult{
		snap: snap, sources: sources, stale: stale, asset: asset, ok: true,
	}, served, quote)
}

// batchChange24h computes the trailing-24h percentage change for a USD-quoted batch row (anchor:
// the USD price 24h ago, as /v1/assets/{id}). Nil when the anchor is unavailable; failures are
// silent-nil by design, so a missing change never costs the caller the price itself.
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

// lookupPriceBatch resolves every id concurrently (bounded by priceBatchConcurrency) and writes the
// envelope. Results land in an index-keyed slice so output keeps first-occurrence order whichever
// worker finishes first. Misses are omitted; the first validation/internal failure in input order
// aborts the batch with a problem+json.
func (s *Server) lookupPriceBatch(w http.ResponseWriter, r *http.Request, ids []string, quote canonical.Asset) {
	ctx := r.Context()
	results := make([]batchRowResult, len(ids))

	// Bounded fan-out: each worker writes its own results[i] slot
	// (disjoint memory — no lock needed) and the semaphore caps how
	// many DB round-trips run at once.
	sem := make(chan struct{}, priceBatchConcurrency)
	var wg sync.WaitGroup
	for i, raw := range ids {
		wg.Add(1)
		go func(i int, raw string) {
			// Registered first so it unwinds LAST — the semaphore
			// release below must run before the panic is swallowed, and
			// wg.Done must run or wg.Wait() deadlocks.
			defer wg.Done()
			// Detached goroutine → middleware.Recoverer does not cover
			// it. results[i] is left at its ZERO value on a panic, whose
			// fail is nil and snap unset, so the id is DROPPED from the
			// response rather than served with a fabricated price.
			defer worker.Recover(s.logger, "api-price-batch-row")
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.resolveBatchRow(ctx, r, raw, quote)
		}(i, raw)
	}
	wg.Wait()

	// Client disconnected mid-batch: nothing to write. A server-side deadline
	// (middleware.RequestTimeout) is NOT a client abort and must fall through to the fail-scan, so
	// writeProblem can answer a retryable 503 instead of net/http's bodyless 200 (see clientAborted).
	if clientAborted(r, nil) {
		return
	}
	// First failure in INPUT order aborts the batch — deterministic
	// regardless of worker completion order.
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

// batchDegraded reports a row dropped by a failed read or a recovered
// worker panic (a zero result) rather than by a genuine miss.
func batchDegraded(results []batchRowResult) bool {
	for i := range results {
		r := &results[i]
		if r.readFailed || (!r.ok && !r.skip && r.fail == nil) {
			return true
		}
	}
	return false
}

// batchEnvelope folds the per-id results (index-aligned with ids) into
// the response envelope: price rows in input order, withheld ids named,
// and each flag the OR over the served rows.
func batchEnvelope(ids []string, results []batchRowResult) Envelope {
	out := make([]PriceSnapshot, 0, len(ids))
	var withheld, thin []string
	allSources := map[string]struct{}{}
	anyStale := false
	anyFrozen := false
	anySingleSource := false
	anyTriangulated := false
	for i := range results {
		row := results[i]
		if !row.ok {
			if row.withheld {
				withheld = append(withheld, ids[i])
			}
			continue // skip — per-asset miss
		}
		if row.stale {
			anyStale = true
		}
		if row.triangulated {
			anyTriangulated = true
		}
		if row.thin {
			thin = append(thin, ids[i])
		}
		for _, src := range row.sources {
			allSources[src] = struct{}{}
		}
		// Per-row freeze (resolved by resolveBatchRow) → OR into the
		// envelope flag. Best-effort: an absent freeze marker means
		// "not frozen".
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
	// Sort for ADR-0015 byte-identical cross-region property —
	// single-asset reads sort sources at the storage boundary
	// (timescale.normalizeVwapSources); the batch endpoint unions
	// per-row sources through a map, whose iteration order would
	// break the byte-identical contract for batch responses.
	sort.Strings(srcs)
	return Envelope{
		Data:     out,
		Sources:  srcs,
		Withheld: withheld,
		Thin:     thin,
		Flags: Flags{
			ThinMarket:   len(thin) > 0,
			Stale:        anyStale,
			Frozen:       anyFrozen,
			Degraded:     anyFrozen,
			SingleSource: anySingleSource,
			Triangulated: anyTriangulated,
		},
	}
}

// ─── Helpers for PriceReader implementations ──────────────────────

// LastTradeToSnapshot converts a canonical.Trade into a PriceSnapshot with price_type="last_trade".
// Price = QuoteAmount / BaseAmount as a decimal string at roundToDecimals precision; callers supply
// `decimals` for the quote asset's scale. ok is false for a trade with a zero leg: it has no price.
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

// VWAP1mToSnapshot is the CAGG-served counterpart to [LastTradeToSnapshot]: it maps a 1-minute
// prices_1m row to a [PriceSnapshot]. `assetID` and `quote` are the request's strings, passed in
// rather than re-derived from the row so the echo stays as the client sent it. `vwap` is the
// NUMERIC column's decimal text, passed through unparsed. `observed_at` is the END of the closed
// window (`bucketStart + 1m`), since the price covers trades that closed during it.
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

// priceRatioDecimal returns QuoteAmount / BaseAmount as a decimal string with `decimals` digits
// after the point, via big.Rat with no float (ADR-0003).
//
// Never panics; ok is false when either leg is not positive. It truncates (floors), never rounds,
// and extends a ratio too small for `decimals` rather than serving zero ([ratToDecimal] renders
// it, so /v1/history and the bucketed surfaces agree digit for digit).
//
// The ratio is in raw stroop units: QuoteAmount=12,420,000 over BaseAmount=1,000,000,000 at
// decimals=7 gives "0.0001242". Callers pick decimals for display; VWAP/OHLC paths store
// pre-scaled prices instead.
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

// parsePriceQuoteParam parses the optional ?quote= (default
// fiat:USD). ok=false means a 400 problem+json was written.
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
