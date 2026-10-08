package confidence

import "math"

// BootstrapDays and BootstrapConfidenceCap are ADR-0019's warmup policy: under 30 days of
// history confidence caps at 0.5, since a new asset's baseline cannot say what is normal yet.
// No calendar age reaches this package, so the cap gates on [BootstrapDensityDays].
const (
	BootstrapDays          = 30.0
	BootstrapConfidenceCap = 0.5
)

// BootstrapDensityDays is [BootstrapDays] in the bucket-density unit this package is handed.
// Density tops out at 30.0 only for a pair trading every minute, so gating at 30.0 never
// released the cap; buckets accrue at most 1,440/day, so 28.5 still proves 28.5 calendar days.
const BootstrapDensityDays = BootstrapDays * bootstrapDensityFraction

// bootstrapDensityFraction is the share of a perfectly observed 30-day window that counts
// as mature; r1's densest pairs run ~99.2%, well inside it.
const bootstrapDensityFraction = 0.95

// BootstrapReengageDensityDays is the gate's lower hysteresis edge: a released pair is
// capped again only below it, absorbing a ~3-day ingestion gap. It cannot release anything.
const BootstrapReengageDensityDays = BootstrapDays * bootstrapReengageFraction

const bootstrapReengageFraction = 0.90

// Inputs are one bucket's raw observations, converted to a [Score] without further IO.
type Inputs struct {
	// ZScore is the largest multi-window baseline z-score; 0 in full bootstrap.
	ZScore float64

	// SourceCount — distinct contributing sources in the bucket.
	SourceCount int

	// SourceClassCount — distinct source CLASSES (CEX / DEX /
	// oracle / aggregator). 0 when no sources contributed.
	SourceClassCount int

	// LiquidityUSD is bucket USD volume, or [LiquidityUnmeasured] when the pair cannot be
	// valued. Zero means measured-and-empty and zeroes the whole score, so never pass 0
	// for an unpriceable pair.
	LiquidityUSD float64

	// CrossOracleDivergencePct is % deviation from the cross-oracle median; negative means
	// no data and yields the ADR-0019 neutral factor.
	CrossOracleDivergencePct float64

	// CrossOracleAgreementCount is how many external references corroborated our VWAP.
	// Transparency only: it is served in [Factors] but not scored. Negative, or a no-data
	// divergence, serves 0.
	CrossOracleAgreementCount int

	// TriangulationChecked gates [TriangulationDivergencePct]; false drops the factor's weight.
	// Unlike the negative sentinel, a float's zero value would read as perfect agreement, and
	// fail-open is the wrong default for a corroboration signal.
	TriangulationChecked bool

	// TriangulationDivergencePct is % deviation between the direct VWAP and a configured
	// triangulation composite (e.g. XLM/USD × USD/EUR). It is corroboration, not a source: it
	// reuses our own legs, so it must never count toward the freeze's source_count leg.
	TriangulationDivergencePct float64

	// BaselineAgeDays is baseline DENSITY in days of 1-minute buckets, not calendar age: a
	// baseline is as trustworthy as the samples behind it. At most [BootstrapDays]; negative
	// means no baseline and the factor returns 0.5.
	BaselineAgeDays float64

	// BootstrapReleased is the previous gate state, selecting the hysteresis edge; false is
	// the conservative upper gate.
	BootstrapReleased bool
}

// Factors is the per-factor decomposition served beside the score, so consumers can see
// why confidence dropped.
type Factors struct {
	ZScore                 float64 `json:"z_score"`
	SourceCount            float64 `json:"source_count"`
	Diversity              float64 `json:"diversity"`
	Liquidity              float64 `json:"liquidity"`
	CrossOracle            float64 `json:"cross_oracle"`
	TriangulationAgreement float64 `json:"triangulation_agreement"`
	BaselineQuality        float64 `json:"baseline_quality"`

	// CrossOracleChecked is true when real cross-oracle data fed CrossOracle; false means
	// the neutral value was used and MUST NOT be read as "references agree".
	CrossOracleChecked bool `json:"cross_oracle_checked"`

	// LiquidityMeasured is true when real USD volume fed Liquidity. Load-bearing: the
	// unmeasured neutral is 0.5, and a measured ~$31.6K bucket also scores 0.5, so 0.5 alone
	// is not evidence of liquidity.
	LiquidityMeasured bool `json:"liquidity_measured"`

	// CrossOracleAgreement is the corroborating-reference count; 0 when unchecked.
	CrossOracleAgreement int `json:"cross_oracle_agreement"`

	// TriangulationChecked is true when a fresh composite was compared. When false the factor
	// is excluded from the score, not just neutral: a normalised geometric mean has no neutral
	// constant, and re-scoring every pair without a chain would be worse than the gap.
	TriangulationChecked bool `json:"triangulation_checked"`

	// BaselineAgeDays is the scored density (see [Inputs.BaselineAgeDays]); negative means none.
	BaselineAgeDays float64 `json:"baseline_age_days"`

	// BootstrapCapped is true when the bootstrap ceiling bounded this score, so the value may
	// be the cap rather than the evidence.
	BootstrapCapped bool `json:"bootstrap_capped"`
}

// Weights are the per-factor exponents of the weighted geometric mean; 0 removes a factor.
type Weights struct {
	ZScore                 float64
	SourceCount            float64
	Diversity              float64
	Liquidity              float64
	CrossOracle            float64
	TriangulationAgreement float64
	BaselineQuality        float64
}

// DefaultWeights is all ones except triangulation at 0.5: a composite reuses our own legs,
// so it corroborates at about half an independent reference. The discount lives here so the
// triangulation and cross-oracle factors stay directly comparable on the wire.
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

// Score is the combined confidence plus its served decomposition.
type Score struct {
	Confidence float64 `json:"confidence"`
	Factors    Factors `json:"factors"`
}

// Compute returns the weighted geometric mean of the factors:
//
//	confidence = prod(factor_i ^ weight_i) ^ (1 / sum(weights))
//
// All-zero weights return 0.5 with factors populated; a zero factor with non-zero weight
// zeroes the score, as ADR-0019 wants.
func Compute(in Inputs, w Weights) Score {
	f := Factors{
		ZScore:                 ZScoreFactor(in.ZScore),
		SourceCount:            SourceCountFactor(in.SourceCount),
		Diversity:              DiversityFactor(in.SourceClassCount),
		Liquidity:              LiquidityFactor(in.LiquidityUSD),
		CrossOracle:            CrossOracleFactor(in.CrossOracleDivergencePct),
		TriangulationAgreement: TriangulationAgreementFactor(triangulationInput(in)),
		BaselineQuality:        BaselineQualityFactor(in.BaselineAgeDays),
		BaselineAgeDays:        servedBaselineAgeDays(in.BaselineAgeDays),
		BootstrapCapped:        bootstrapCapInForce(in.BaselineAgeDays, in.BootstrapReleased),
	}
	// A negative LiquidityUSD is the unvalued sentinel.
	f.LiquidityMeasured = in.LiquidityUSD >= 0
	// Unchecked (negative or NaN divergence) forces the agreement count to 0, since
	// unchecked is not zero agreement.
	if in.CrossOracleDivergencePct >= 0 && !math.IsNaN(in.CrossOracleDivergencePct) {
		f.CrossOracleChecked = true
		if in.CrossOracleAgreementCount > 0 {
			f.CrossOracleAgreement = in.CrossOracleAgreementCount
		}
	}
	// Unchecked also zeroes the weight, so a pair with no chain is not re-scored.
	triWeight := 0.0
	if in.TriangulationChecked && in.TriangulationDivergencePct >= 0 && !math.IsNaN(in.TriangulationDivergencePct) {
		f.TriangulationChecked = true
		triWeight = w.TriangulationAgreement
	}

	totalWeight := w.ZScore + w.SourceCount + w.Diversity + w.Liquidity + w.CrossOracle + triWeight + w.BaselineQuality
	if totalWeight <= 0 {
		return Score{Confidence: 0.5, Factors: f}
	}

	// Sum logs so a tiny factor cannot underflow the product.
	logSum := weightedLog(f.ZScore, w.ZScore) +
		weightedLog(f.SourceCount, w.SourceCount) +
		weightedLog(f.Diversity, w.Diversity) +
		weightedLog(f.Liquidity, w.Liquidity) +
		weightedLog(f.CrossOracle, w.CrossOracle) +
		weightedLog(f.TriangulationAgreement, triWeight) +
		weightedLog(f.BaselineQuality, w.BaselineQuality)

	conf := math.Exp(logSum / totalWeight)
	conf = applyBootstrapCap(conf, in.BaselineAgeDays, in.BootstrapReleased)
	return Score{Confidence: clamp01(conf), Factors: f}
}

// triangulationInput keeps the unchecked→sentinel mapping in one place so the served
// factor and Compute's weight-zeroing cannot disagree.
func triangulationInput(in Inputs) float64 {
	if !in.TriangulationChecked {
		return -1
	}
	return in.TriangulationDivergencePct
}

// applyBootstrapCap caps confidence while the baseline is thin. A negative age (no
// baseline) is capped too; NaN is not, as BaselineQuality already drags it down.
func applyBootstrapCap(c, ageDays float64, released bool) float64 {
	if !bootstrapCapInForce(ageDays, released) {
		return c
	}
	if c > BootstrapConfidenceCap {
		return BootstrapConfidenceCap
	}
	return c
}

// bootstrapCapInForce is the one predicate behind both the ceiling in
// [applyBootstrapCap] and [Factors.BootstrapCapped], so the served flag
// can never disagree with the cap that was applied.
func bootstrapCapInForce(ageDays float64, released bool) bool {
	if math.IsNaN(ageDays) {
		return false
	}
	if released {
		return ageDays < BootstrapReengageDensityDays
	}
	return ageDays < BootstrapDensityDays
}

// servedBaselineAgeDays maps a non-finite density onto the negative
// no-baseline reading: the Score is JSON-encoded into the cache, and
// encoding/json rejects NaN and ±Inf.
func servedBaselineAgeDays(ageDays float64) float64 {
	if math.IsNaN(ageDays) || math.IsInf(ageDays, 0) {
		return -1
	}
	return ageDays
}

// safeLog maps log of zero, negative or NaN to -Inf so the geometric mean goes to zero.
func safeLog(x float64) float64 {
	if x <= 0 || math.IsNaN(x) {
		return math.Inf(-1)
	}
	return math.Log(x)
}

// weightedLog returns 0 for a zero weight: -Inf * 0 is NaN, which reached clamp01 as
// confidence 0 beside healthy factors. A zero-weighted factor is removed, as documented.
func weightedLog(factor, weight float64) float64 {
	if weight == 0 {
		return 0
	}
	return safeLog(factor) * weight
}
