package confidence

import "math"

// Factor shapes follow ADR-0019 and are code, not config: only the per-factor
// [Weights] are operator-tunable.

// zScoreSigmoidWidth controls how fast [ZScoreFactor] decays with
// rising z. The ADR's "1.0 at z=0, decays smoothly to ~0 at z=10"
// shape is achieved by 1 / (1 + exp((z - 5) / width)) with width
// chosen so the value at z=10 is ~0 and z=0 is ~1.
const zScoreSigmoidWidth = 1.0

// sourceCountInflectionN is the n_sources value at which
// [SourceCountFactor] crosses 0.5. Per the ADR: "single-source
// assets cap at ~0.3, n>=6 reaches near-1.0" — the crossover at
// n=3 satisfies both.
const sourceCountInflectionN = 3.0

// liquidityFloorUSD and liquidityCeilingUSD bound [LiquidityFactor]: $1K → 0, $1M → ~1.
// At $100K the median BTC/USD 5m bucket (~$124K) already saturated the factor, so it
// discriminated only among thin pairs and wash volume bought full credit cheaply.
// The ceiling moves which freeze leg binds below ~$15.6K, not the z > 5 trigger itself.
const (
	liquidityFloorUSD   = 1_000.0
	liquidityCeilingUSD = 1_000_000.0
)

// LiquidityUnmeasured is the [Inputs.LiquidityUSD] sentinel for "could not value in USD",
// as opposed to "measured and thin"; any negative value counts, as in the other factors.
const LiquidityUnmeasured = -1.0

// LiquidityUnmeasuredFactor is the neutral no-signal midpoint for [LiquidityUnmeasured].
// Deliberately not tracked to the curve (LiquidityFactor(10_000) = 0.333): the unmeasured
// pairs are the thin single-source ones where a false freeze is most damaging. Read
// [Factors.LiquidityMeasured], since a measured ~$31.6K bucket also scores 0.5.
const LiquidityUnmeasuredFactor = 0.5

// crossOracleTolerancePct: within 1% of the cross-oracle median is full agreement (ADR-0019).
const crossOracleTolerancePct = 1.0

// crossOracleHalfLifePct puts a 5% deviation near 0.5.
const crossOracleHalfLifePct = 4.0

// triangulationTolerancePct is wider than [crossOracleTolerancePct] to absorb the
// composite's own error: its FX leg snaps to daily quotes up to days old, so ordinary
// EUR/USD drift against a stale leg would otherwise read as disagreement.
const triangulationTolerancePct = 2.0

// triangulationHalfLifePct matches [crossOracleHalfLifePct] so the two corroboration
// factors stay comparable; a 20% gap takes the factor to ~0.05.
const triangulationHalfLifePct = 4.0

// triangulationNeutralFactor is the no-composite value; [Compute] zeroes its weight anyway.
const triangulationNeutralFactor = 0.7

// baselineFullDays is the age at which a baseline is fully mature (ADR-0019).
const baselineFullDays = 30.0

// ZScoreFactor maps a baseline z-score to [0, 1]: 1.0 at z=0, ~0.5 at z=5, ~0 at z=10.
// Negative, NaN and Inf clamp to 0.
func ZScoreFactor(z float64) float64 {
	if math.IsNaN(z) || z < 0 {
		return 0
	}
	if math.IsInf(z, 0) {
		return 0
	}
	return clamp01(1.0 / (1.0 + math.Exp((z-5.0)/zScoreSigmoidWidth)))
}

// SourceCountFactor is 1/(1+exp(-(n-3))): one source ~0.3, n>=6 near 1.0; n < 0 is 0.
func SourceCountFactor(n int) float64 {
	if n < 0 {
		return 0
	}
	return clamp01(1.0 / (1.0 + math.Exp(-(float64(n) - sourceCountInflectionN))))
}

// DiversityFactor is 0 for no class, 0.5 for one, 1.0 for two or more: CEX + DEX
// agreement beats three CEXs that may share an upstream feed bug.
func DiversityFactor(classCount int) float64 {
	switch {
	case classCount <= 0:
		return 0
	case classCount == 1:
		return 0.5
	default:
		return 1.0
	}
}

// LiquidityFactor log-interpolates USD bucket volume between the floor (0) and ceiling (1).
// A negative volume is [LiquidityUnmeasured] and reads neutral, but a measured near-zero
// stays 0: it is the real signal the freeze needs, and collapsing the two pinned the
// freeze's confidence leg true for every pair not valued in USD. NaN returns 0.
func LiquidityFactor(usdVolume float64) float64 {
	if math.IsNaN(usdVolume) {
		return 0
	}
	if usdVolume < 0 {
		return LiquidityUnmeasuredFactor
	}
	if usdVolume >= liquidityCeilingUSD {
		return 1.0
	}
	if usdVolume <= liquidityFloorUSD {
		return 0
	}
	// Log-interpolation: how far between log(floor) and log(ceiling)
	// is log(volume), as a fraction in [0, 1].
	return clamp01(
		(math.Log(usdVolume) - math.Log(liquidityFloorUSD)) /
			(math.Log(liquidityCeilingUSD) - math.Log(liquidityFloorUSD)),
	)
}

// CrossOracleFactor is 1.0 within [crossOracleTolerancePct], then decays exponentially
// (5% → ~0.5, 10% → ~0.21). A negative divergence means no cross-oracle data and returns
// the ADR-0019 neutral 0.7, so assets without external references are not dragged down.
func CrossOracleFactor(divergencePct float64) float64 {
	if math.IsNaN(divergencePct) {
		return 0
	}
	if divergencePct < 0 {
		// Sentinel: "no cross-oracle data". Return neutral.
		return 0.7
	}
	if divergencePct <= crossOracleTolerancePct {
		return 1.0
	}
	// Exponential decay past the tolerance band:
	//   factor = exp(-(x - tolerance) * ln(2) / half_life)
	// Hits 0.5 at x = tolerance + half_life.
	excess := divergencePct - crossOracleTolerancePct
	return clamp01(math.Exp(-excess * math.Ln2 / crossOracleHalfLifePct))
}

// TriangulationAgreementFactor scores a direct price against its triangulated composite,
// shaped like [CrossOracleFactor] (2% → 1.0, 6% → ~0.5, 20% → ~0.05). Divergence decays
// hard rather than reverting to neutral: on mostly single-source pairs the thin direct
// print is the likelier liar. Negative means no composite; NaN returns 0.
func TriangulationAgreementFactor(divergencePct float64) float64 {
	if math.IsNaN(divergencePct) {
		return 0
	}
	if divergencePct < 0 {
		// Sentinel: "no composite for this pair". Return neutral.
		return triangulationNeutralFactor
	}
	if divergencePct <= triangulationTolerancePct {
		return 1.0
	}
	excess := divergencePct - triangulationTolerancePct
	return clamp01(math.Exp(-excess * math.Ln2 / triangulationHalfLifePct))
}

// BaselineQualityFactor ramps 0.5 → 1.0 over 30 days-equivalent of 1-minute samples
// (density, not calendar age, so a sparse pair stays low). Negative or NaN means
// untrained and returns the same 0.5 as zero history.
func BaselineQualityFactor(daysHistory float64) float64 {
	if math.IsNaN(daysHistory) || daysHistory < 0 {
		return 0.5
	}
	if daysHistory >= baselineFullDays {
		return 1.0
	}
	// Linear ramp from 0.5 → 1.0 across [0, 30] days.
	return 0.5 + 0.5*(daysHistory/baselineFullDays)
}

// clamp01 clamps x to [0, 1] so a math edge case cannot poison the geometric mean.
func clamp01(x float64) float64 {
	if math.IsNaN(x) {
		return 0
	}
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
