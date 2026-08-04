package confidence

import "math"

// BootstrapDays is the age threshold below which the confidence
// score is hard-capped at [BootstrapConfidenceCap], regardless of
// other factor values. Per ADR-0019 §"Bootstrap (warmup) policy
// for new assets":
//
//	"For an asset with < 30 days of history: ... cap confidence at
//	 0.5 regardless of other factors."
//
// The cap exists because a freshly-listed asset's per-asset
// baseline isn't trustworthy yet — even with multi-source
// agreement and tight liquidity, we lack the historical signal
// to know what's normal for THIS asset. 0.5 says "we can serve
// the price, but consumers should treat it as provisional".
const (
	BootstrapDays          = 30.0
	BootstrapConfidenceCap = 0.5
)

// Inputs are the raw observations a single bucket carries. The
// orchestrator populates this from the bucket's stats + the per-
// asset baseline; this package converts to a [Score] without any
// further IO.
//
// Field shapes deliberately match the data the orchestrator
// already has at confidence-compute time — no new accessors needed
// at the call site.
type Inputs struct {
	// ZScore — the largest z-score across the multi-window
	// baselines (`baseline.MultiBaseline.MaxZScore`). Pass 0 when
	// the asset is in full bootstrap (no baseline at any window).
	ZScore float64

	// SourceCount — distinct contributing sources in the bucket.
	SourceCount int

	// SourceClassCount — distinct source CLASSES (CEX / DEX /
	// oracle / aggregator). 0 when no sources contributed.
	SourceClassCount int

	// LiquidityUSD — bucket volume in USD, or [LiquidityUnmeasured]
	// (negative) when the caller cannot value the pair in USD at all.
	//
	// Zero is NOT the unmeasured state: it means "measured, and this
	// bucket carried no USD volume", which zeroes [LiquidityFactor]
	// and — the geometric mean being dominated by any zero factor —
	// the whole score. Pass the sentinel rather than 0 for an
	// unpriceable pair (COR-14).
	LiquidityUSD float64

	// CrossOracleDivergencePct — % absolute deviation between our
	// price and the cross-oracle median. Pass a negative value to
	// signal "no cross-oracle data" (returns the neutral factor
	// per ADR-0019 worked example).
	CrossOracleDivergencePct float64

	// CrossOracleAgreementCount — how many independent external
	// references corroborated our VWAP within the divergence
	// threshold at refresh time (ADR-0019 Phase 3;
	// divergence.CachedResult.AgreementCount). Transparency-only:
	// it does NOT enter the combined score (the ADR's
	// cross_oracle_factor input is divergence-from-median), but it
	// ships in the served [Factors] decomposition so consumers can
	// gate on corroboration strength directly. Pass a negative
	// value when cross-oracle data is unavailable — served as 0
	// alongside CrossOracleChecked=false. Ignored (forced to 0 on
	// the wire) when CrossOracleDivergencePct carries the no-data
	// sentinel.
	CrossOracleAgreementCount int

	// TriangulationChecked gates [TriangulationDivergencePct]. False —
	// the ZERO VALUE — means "no composite was compared", and [Compute]
	// then drops the factor's weight entirely, so an un-triangulated
	// pair scores exactly as it did before this input existed.
	//
	// This is the one place this package does NOT mirror
	// [CrossOracleDivergencePct]'s negative-sentinel-only shape, and the
	// deviation is deliberate: a float's zero value is 0.0, which on the
	// sentinel shape reads as "checked, and the composite agrees
	// perfectly" — full credit, awarded to every caller that never heard
	// of this field. Fail-open is the wrong default for a corroboration
	// signal, and it is the same category of defect as COR-14 (a
	// measured 0 that was really "not measured"). An explicit flag makes
	// omission read as ignorance instead of as evidence.
	TriangulationChecked bool

	// TriangulationDivergencePct — % absolute deviation between the
	// pair's DIRECT price (this bucket's VWAP) and the COMPOSITE price
	// implied by a configured triangulation chain for the same pair
	// (e.g. XLM/EUR direct vs XLM/USD × USD/EUR). Read only when
	// [TriangulationChecked] is true; a negative value is treated as
	// unchecked as well, so the sentinel convention still holds for
	// callers that use it.
	//
	// Why this is a confidence input and not a source: a composite is
	// CORROBORATION, not a second venue. It re-uses our own leg VWAPs,
	// our own pipeline and (usually) our own upstream sources, so it
	// cannot carry the independence that [Inputs.SourceCount] asserts.
	// It therefore feeds this factor only — the freeze's
	// `source_count <= 1` leg (ADR-0019's 3-signal AND) must NOT count
	// a triangulated price as a corroborating source, or the AND
	// silently degrades to two signals on exactly the thin pairs
	// triangulation is deployed for. See
	// orchestrator.triangulationDivergencePct for the producer side.
	//
	// Direction: agreement gives full credit (1.0) and, being an extra
	// factor in a normalised geometric mean, lifts the score; a large
	// divergence is a manipulation signal on one side or the other and
	// decays the factor toward 0, dragging the score down.
	TriangulationDivergencePct float64

	// BaselineAgeDays — days-equivalent of baseline DENSITY, not
	// calendar age (COR-14). The only production caller
	// (orchestrator.baselineAgeDays) passes Day30.N / 1440, i.e. how
	// many 1-minute buckets of real history back the 30d window
	// expressed in days-worth-of-buckets; a pair that trades in 200
	// buckets a day reads as 0.14 "days" no matter how many calendar
	// months it has existed.
	//
	// That is deliberate — a baseline is trustworthy in proportion to
	// the samples that fed its median/MAD, not to how long ago it was
	// first written — but the NAME says age, so read the factor's
	// output as "how well-supported is this baseline", never as
	// "how old is this asset". 0 = no support (bootstrap penalty);
	// negative means "no baseline at all" and the factor returns 0.5.
	BaselineAgeDays float64
}

// Factors holds the per-factor decomposition that ships on the
// wire alongside the combined confidence score. Customers and
// operators look at this to understand WHY confidence dropped:
// "z=1.0 (ok), src=0.3 (single-source), div=0.5 (one class)" tells
// you the issue is source coverage, not staleness.
type Factors struct {
	ZScore                 float64 `json:"z_score"`
	SourceCount            float64 `json:"source_count"`
	Diversity              float64 `json:"diversity"`
	Liquidity              float64 `json:"liquidity"`
	CrossOracle            float64 `json:"cross_oracle"`
	TriangulationAgreement float64 `json:"triangulation_agreement"`
	BaselineQuality        float64 `json:"baseline_quality"`

	// CrossOracleChecked disambiguates the CrossOracle factor value
	// per the CS-087 DivergenceChecked discipline: true means real
	// cross-oracle data fed the factor; false means the neutral
	// no-data value was used. Without it a consumer cannot tell
	// CrossOracle=0.7 "unverified" from CrossOracle=0.7 "verified,
	// mildly diverging" — and MUST NOT read false as "references
	// agree".
	CrossOracleChecked bool `json:"cross_oracle_checked"`

	// LiquidityMeasured disambiguates the Liquidity factor value on
	// exactly the same CS-087 discipline as CrossOracleChecked above:
	// true means a real USD volume fed the factor, false means the
	// neutral [LiquidityUnmeasuredFactor] was substituted because the
	// pair could not be valued in USD.
	//
	// This is load-bearing, not symmetry for its own sake. The neutral
	// is 0.5, and SOME measured bucket always maps to 0.5 too — the
	// log-midpoint of the factor's own band. Until 2026-07-25 that
	// collision was at its worst: the ceiling was $100K, which put the
	// midpoint at exactly $10,000 — the production `min_usd_volume`
	// floor, i.e. the single most likely measured value, since
	// dropForMinUSDVolume rejects anything below it before confidence is
	// computed. Raising the ceiling to $1M moved the collision volume to
	// ≈ $31,623 and the publish floor now reads 0.333, so the two states
	// are further apart in practice — but they are NOT distinguishable
	// from the number alone, and a consumer that cannot tell them apart
	// still MUST NOT read 0.5 as evidence of real liquidity.
	LiquidityMeasured bool `json:"liquidity_measured"`

	// CrossOracleAgreement is the count of independent external
	// references that corroborated our price within the divergence
	// threshold (ADR-0019 Phase 3 cross-oracle agreement). Always 0
	// when CrossOracleChecked is false — read it only when checked.
	CrossOracleAgreement int `json:"cross_oracle_agreement"`

	// TriangulationChecked disambiguates the TriangulationAgreement
	// factor on the same CS-087 discipline as CrossOracleChecked: true
	// means a real composite price (a configured triangulation chain's
	// fresh output for this pair) was compared against the direct price;
	// false means no composite was available and the neutral placeholder
	// was served. false MUST NOT be read as "the composite agrees".
	//
	// One difference from CrossOracleChecked, and it is deliberate: when
	// this is false the factor is EXCLUDED from the combined score
	// entirely (its weight is zeroed in [Compute]), so the served value
	// is inert rather than merely neutral. A normalised geometric mean
	// has no truly neutral constant — adding any factor value changes
	// every score through the 1/sum(weights) exponent — and a
	// corroboration signal that silently re-scored every pair without a
	// chain would be a worse defect than the gap it fills.
	TriangulationChecked bool `json:"triangulation_checked"`
}

// Weights are the per-factor exponents in the weighted geometric
// mean. ADR-0019 specifies these as operator-tunable but defaults
// them all to 1.0 (unweighted geometric mean). A weight of 0
// effectively removes that factor from the product.
//
// Operators tune these via [anomaly.weights] in TOML — that wiring
// lands with the orchestrator slice. This struct is just the math
// surface.
type Weights struct {
	ZScore                 float64
	SourceCount            float64
	Diversity              float64
	Liquidity              float64
	CrossOracle            float64
	TriangulationAgreement float64
	BaselineQuality        float64
}

// DefaultWeights returns the ADR-0019 default — all ones except the
// triangulation-agreement factor, which defaults to 0.5.
//
// The half weight IS the "a derived path is not an independent venue"
// discount, and it lives here rather than in the factor's ceiling on
// purpose: [TriangulationAgreementFactor] keeps the same shape as
// [CrossOracleFactor] so the two decomposition values are directly
// comparable on the wire ("0.8 divergence-decayed" means the same thing
// in both columns), and the evidence class is expressed where this
// package already expresses relative influence. A composite re-uses our
// own leg VWAPs and pipeline, so it corroborates at roughly half the
// weight of an independent external reference; it never contributes to
// [Inputs.SourceCount] at all.
func DefaultWeights() Weights {
	return Weights{
		ZScore:                 1.0,
		SourceCount:            1.0,
		Diversity:              1.0,
		Liquidity:              1.0,
		CrossOracle:            1.0,
		TriangulationAgreement: 0.5,
		BaselineQuality:        1.0,
	}
}

// Score is the combined confidence score plus its decomposition.
// The wire response carries this whole struct — Confidence on the
// envelope, Factors on a sibling field for transparency.
type Score struct {
	Confidence float64 `json:"confidence"`
	Factors    Factors `json:"factors"`
}

// Compute returns the [Score] for a bucket given its raw inputs and
// per-factor weights. Pass [DefaultWeights] to compute with the
// ADR-0019 default unweighted shape.
//
// The combined score is the weighted geometric mean:
//
//	confidence = prod(factor_i ^ weight_i) ^ (1 / sum(weights))
//
// The 1/sum(weights) normalisation keeps the final value in [0, 1]
// regardless of weight magnitude. Without it, doubling every weight
// would square the result.
//
// Edge cases:
//
//   - All weights = 0: returns a neutral 0.5 with the per-factor
//     decomposition still populated. (Useful for diagnostics:
//     "compute the factors but ignore the combiner".)
//   - Any factor returns exactly 0 with non-zero weight: the
//     geometric mean is 0 (the dominating-factor behaviour the
//     ADR explicitly wants).
func Compute(in Inputs, w Weights) Score {
	f := Factors{
		ZScore:                 ZScoreFactor(in.ZScore),
		SourceCount:            SourceCountFactor(in.SourceCount),
		Diversity:              DiversityFactor(in.SourceClassCount),
		Liquidity:              LiquidityFactor(in.LiquidityUSD),
		CrossOracle:            CrossOracleFactor(in.CrossOracleDivergencePct),
		TriangulationAgreement: TriangulationAgreementFactor(triangulationInput(in)),
		BaselineQuality:        BaselineQualityFactor(in.BaselineAgeDays),
	}
	// Mirrors the CrossOracleChecked branch below: a negative
	// LiquidityUSD is the "could not value this pair in USD" sentinel,
	// so the served decomposition marks it unmeasured.
	f.LiquidityMeasured = in.LiquidityUSD >= 0
	// Checked mirrors CrossOracleFactor's sentinel branch exactly:
	// a negative divergence means "no cross-oracle data" (neutral
	// factor), so the served decomposition marks unchecked and the
	// agreement count is forced to 0 (unchecked ≠ zero agreement —
	// consumers read the pair together per CS-087). NaN divergence
	// (defensive-zero factor) also reads as unchecked.
	if in.CrossOracleDivergencePct >= 0 && !math.IsNaN(in.CrossOracleDivergencePct) {
		f.CrossOracleChecked = true
		if in.CrossOracleAgreementCount > 0 {
			f.CrossOracleAgreement = in.CrossOracleAgreementCount
		}
	}
	// Same sentinel branch for the composite comparison — but the
	// unchecked case also drops the factor's WEIGHT to zero, which is
	// what makes "no chain configured for this pair" a genuine no-op
	// rather than a re-scoring of every pair in the index. See
	// [Factors.TriangulationChecked].
	triWeight := 0.0
	if in.TriangulationChecked && in.TriangulationDivergencePct >= 0 && !math.IsNaN(in.TriangulationDivergencePct) {
		f.TriangulationChecked = true
		triWeight = w.TriangulationAgreement
	}

	totalWeight := w.ZScore + w.SourceCount + w.Diversity + w.Liquidity + w.CrossOracle + triWeight + w.BaselineQuality
	if totalWeight <= 0 {
		return Score{Confidence: 0.5, Factors: f}
	}

	// Sum log-factors instead of multiplying directly — keeps the
	// arithmetic numerically stable when any factor is very small
	// (log(small) is large negative; the exp at the end recovers
	// the result without underflow).
	logSum := weightedLog(f.ZScore, w.ZScore) +
		weightedLog(f.SourceCount, w.SourceCount) +
		weightedLog(f.Diversity, w.Diversity) +
		weightedLog(f.Liquidity, w.Liquidity) +
		weightedLog(f.CrossOracle, w.CrossOracle) +
		weightedLog(f.TriangulationAgreement, triWeight) +
		weightedLog(f.BaselineQuality, w.BaselineQuality)

	conf := math.Exp(logSum / totalWeight)
	conf = applyBootstrapCap(conf, in.BaselineAgeDays)
	return Score{Confidence: clamp01(conf), Factors: f}
}

// triangulationInput collapses the (Checked, DivergencePct) pair into
// the single value [TriangulationAgreementFactor] takes: the raw
// divergence when a composite really was compared, the negative
// no-data sentinel otherwise. Keeps the unchecked→neutral mapping in
// ONE place so the served factor value and the weight-zeroing branch
// in [Compute] can never disagree about what "unchecked" means.
func triangulationInput(in Inputs) float64 {
	if !in.TriangulationChecked {
		return -1
	}
	return in.TriangulationDivergencePct
}

// applyBootstrapCap caps the final confidence at
// [BootstrapConfidenceCap] when the asset is still in bootstrap
// (BaselineAgeDays known and below [BootstrapDays]).
//
// A negative BaselineAgeDays is the "no baseline yet" sentinel —
// stricter than bootstrap, so we apply the cap there too. Callers
// who pass an unknown age via NaN get no cap (the BaselineQuality
// factor already returns 0.5 for NaN, dragging the combiner down
// without a hard ceiling).
func applyBootstrapCap(c, ageDays float64) float64 {
	if math.IsNaN(ageDays) {
		return c
	}
	if ageDays >= BootstrapDays {
		return c
	}
	if c > BootstrapConfidenceCap {
		return BootstrapConfidenceCap
	}
	return c
}

// safeLog returns log(x) with log(0) → -Inf clamped through Exp;
// log(NaN) and log(<0) return -Inf so the geometric mean dominates
// to zero. Defensive: factor outputs are already clamped to [0, 1]
// but the math would produce NaN on log(0) so we route through
// math.Inf(-1) explicitly.
func safeLog(x float64) float64 {
	if x <= 0 || math.IsNaN(x) {
		return math.Inf(-1)
	}
	return math.Log(x)
}

// weightedLog is safeLog(factor) * weight with the one product IEEE-754
// gets wrong made explicit: -Inf * 0 is NaN, not 0.
//
// safeLog returns -Inf for a zero factor, so a factor of exactly 0 that
// happens to carry a weight of 0 produced NaN — which propagates through
// the sum, survives math.Exp, passes both branches of applyBootstrapCap
// untouched, and lands in clamp01(NaN) = 0. A fully healthy pair would
// publish confidence 0 beside a confidence_factors decomposition showing
// every factor near 1.
//
// That contradicts three documented promises in this package: that "a
// weight of 0 effectively removes that factor from the product", that a
// zero factor zeroes the mean only "with non-zero weight", and doc.go's
// flat guarantee that "the geometric mean never produces NaN". A
// zero-weighted factor is REMOVED, which is what the docs already say
// (cold audit 2026-08-04).
func weightedLog(factor, weight float64) float64 {
	if weight == 0 {
		return 0
	}
	return safeLog(factor) * weight
}
