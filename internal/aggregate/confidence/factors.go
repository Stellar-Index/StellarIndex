package confidence

import "math"

// Factor knobs. Public so operators can tune via [anomaly.weights]
// config — but all defaults map to the ADR-0019 documented shapes,
// and the per-factor functions intentionally do NOT take tuning
// parameters: changing the SHAPE of a factor is a code change, not
// a config change. Only the per-factor weights in [Weights] are
// operator-tunable at runtime.

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

// liquidityFloorUSD and liquidityCeilingUSD bound [LiquidityFactor]'s
// log-saturating shape. Below the floor the factor is ~0; above the
// ceiling it's ~1. ADR-0019 specified $1K → 0, $100K → ~1; the ceiling
// was raised to $1M on 2026-07-25 (see the ADR amendment).
//
// Why $100K was the wrong ceiling: it is not a "deep bucket", it is
// roughly a typical one. Measured on the live index, BTC/USD's 5m
// bucket volume has p50 ≈ $123,678 — the MEDIAN bucket of our
// deepest pair already saturates the factor at 1.0, so the factor
// carried no information across the entire top half of its own
// population, and $100K of wash volume bought a manipulator the same
// full credit as $10M of real depth. A ceiling that the median clears
// is a ceiling that only ever discriminates among the thin.
//
// $1M keeps the same log-saturating shape one decade higher, so the
// deep end regains resolution — LiquidityFactor(123_678) reads 0.697
// instead of a pinned 1.0, and a wash-trader now needs a full order
// of magnitude more volume to buy the same credit — while the thin
// end is unchanged in kind: the floor, and the "measured zero → 0"
// rule the Phase 2 freeze leans on, are untouched.
//
// Freeze-band impact, re-derived against the shipped combiner for the
// population that can actually freeze (single source, one class, no
// cross-oracle data, mature baseline, confidence_max_freeze = 0.45):
// the z at which the CONFIDENCE leg of the 3-signal AND crosses moves
// from z ≈ 5.54 (at $12K measured volume) / 6.39 ($100K) to z ≈ 4.79 /
// 5.85. That does NOT move the freeze's trigger point: `z_score > 5.0`
// is a separate, independently-evaluated leg of the same AND
// (phase2FreezeFires), so nothing can freeze below z = 5 whatever the
// confidence leg says. What it moves is WHICH leg binds — below
// ≈ $15.6K of measured volume the confidence leg now goes true before
// z reaches 5, so for that thin slice the AND leans on z + source_count.
// The 8 non-USD-quoted default pairs — the population at highest
// false-freeze risk — are not affected at all: they pass
// [LiquidityUnmeasured] and read [LiquidityUnmeasuredFactor], which
// this change deliberately leaves at 0.5 (see its doc comment).
const (
	liquidityFloorUSD   = 1_000.0
	liquidityCeilingUSD = 1_000_000.0
)

// LiquidityUnmeasured is the [Inputs.LiquidityUSD] sentinel for
// "this bucket's USD volume could not be measured at all" — as
// opposed to "measured, and it is thin". Callers that cannot value a
// pair in USD (the orchestrator's approxUSDVolume for a non-USD-quoted
// pair) pass this instead of 0.
//
// Any negative value is treated as the sentinel, mirroring the
// [CrossOracleFactor] / [BaselineQualityFactor] no-data convention
// this package already uses; the named constant exists so call sites
// read as intent rather than as a magic -1.
const LiquidityUnmeasured = -1.0

// LiquidityUnmeasuredFactor is what [LiquidityFactor] returns for
// [LiquidityUnmeasured]. Neutral — it neither credits nor penalises a
// pair for a measurement we never took.
//
// 0.5 is the same "no signal" midpoint [BaselineQualityFactor] uses
// for its own no-data sentinel (and is stricter than
// [CrossOracleFactor]'s 0.7, which is anchored to an ADR-0019 worked
// example this factor has no counterpart for).
//
// It is NOT tracked to the curve. Until 2026-07-25 this constant had a
// second justification — `LiquidityFactor(10_000)` was also exactly 0.5,
// and 10_000 is the production `min_usd_volume` floor, i.e. the single
// most likely measured value — which made 0.5 the natural midpoint in
// two independent senses. Raising [liquidityCeilingUSD] to $1M broke
// the coincidence: the floor now reads LiquidityFactor(10_000) = 0.333,
// and the curve's log-midpoint moved to sqrt(1_000 × 1_000_000) ≈
// $31,623.
//
// Following the curve down to 0.333 was considered and DELIBERATELY
// rejected. The unmeasured population is the 8 non-USD-quoted pairs in
// the aggregator's default set, which are exactly the thin, single-
// source pairs where a false freeze is most likely and most damaging
// (a false freeze serves a stale last-known-good price — its own money
// bug, see MNY-22). Dropping their only substitute factor by a third
// would lower their confidence for a reason that says nothing about
// them, pushing the Phase 2 confidence leg true more readily for the
// worst-suited population. 0.5 survives as what it always primarily
// was: the deliberate no-signal midpoint of the [0, 1] range, chosen
// for its neutrality rather than for its position on a curve whose
// ceiling is a tuning decision.
//
// The consequence for consumers is a GOOD one and is the reason
// [Factors.LiquidityMeasured] exists: 0.5 no longer collides with the
// factor value of a real bucket at the publish floor, so the two
// states are further apart on the wire than they used to be. Read the
// flag anyway — 0.5 is still reachable by a measured ≈ $31.6K bucket.
const LiquidityUnmeasuredFactor = 0.5

// crossOracleTolerancePct is the deviation below which the factor
// reads as full agreement (1.0). Per ADR-0019: "1.0 when within 1%
// of cross-oracle median".
const crossOracleTolerancePct = 1.0

// crossOracleHalfLifePct is the divergence beyond the tolerance at
// which the exponential decay reaches 0.5. Tuned so a 5% deviation
// (4 percentage points past tolerance) lands near 0.5 — matches
// the ADR's "decays with divergence" intent without prescribing a
// specific point.
const crossOracleHalfLifePct = 4.0

// triangulationTolerancePct is the direct-vs-composite deviation
// below which [TriangulationAgreementFactor] reads as full agreement.
//
// Wider than [crossOracleTolerancePct] (1%) on purpose, and the extra
// point is not slack — it is the composite's own measurement error.
// A chained-fiat leg is snapped to the FX quote at-or-before bucket
// end (ADR-0018's across-region determinism rule), and the fx_quotes
// feed this repo runs writes DAILY buckets with a 7-day accepted
// lookback (timescale.fxQuotesSnapLookback). So the FX leg of a
// composite is routinely up to a day old and, across a holiday
// weekend, several — while the direct price is seconds old. EUR/USD
// moving its ordinary ~0.5%/day against a stale leg would otherwise
// read as "disagreement" every Monday morning.
const triangulationTolerancePct = 2.0

// triangulationHalfLifePct is the divergence past the tolerance at
// which the factor decays to 0.5 — same 4 percentage points as
// [crossOracleHalfLifePct], so the two corroboration factors decay at
// the same rate and their served values stay comparable. A 6% gap
// between a direct price and its composite halves the factor; a 20%
// gap (the shape of a single-venue manipulation on a thin pair)
// takes it to ~0.05.
const triangulationHalfLifePct = 4.0

// triangulationNeutralFactor is what [TriangulationAgreementFactor]
// returns for the "no composite available" sentinel — the same 0.7
// neutral [CrossOracleFactor] uses, for readability. Unlike that one
// it never enters the score: [Compute] zeroes the factor's weight when
// unchecked (see [Factors.TriangulationChecked]).
const triangulationNeutralFactor = 0.7

// baselineFullDays is the age (in days) at which a baseline is
// considered fully mature. ADR §"baseline_quality_factor: 0.5 with
// no baseline data, ramps to 1.0 over the first 30 days".
const baselineFullDays = 30.0

// ZScoreFactor maps a baseline z-score to a confidence factor in
// [0, 1]. Sigmoid-shaped: 1.0 at z=0, ~0.5 at z=5 (the ADR-0019
// anomaly threshold), ~0 at z=10.
//
// Negative or NaN/Inf z values clamp to 0 (defensive — the math
// for z is |x - median| / mad which can't be negative under our
// invariants, but a future caller bug shouldn't propagate to
// confidence).
func ZScoreFactor(z float64) float64 {
	if math.IsNaN(z) || z < 0 {
		return 0
	}
	if math.IsInf(z, 0) {
		return 0
	}
	return clamp01(1.0 / (1.0 + math.Exp((z-5.0)/zScoreSigmoidWidth)))
}

// SourceCountFactor maps n_sources to a confidence factor.
// Logistic shape: 1/(1+exp(-(n-3))). Single-source caps at ~0.3,
// n=3 is 0.5, n>=6 nears 1.0.
//
// n < 0 clamps to 0. NaN inputs (from a future bug at the caller)
// return 0 rather than NaN-poisoning the geometric mean.
func SourceCountFactor(n int) float64 {
	if n < 0 {
		return 0
	}
	return clamp01(1.0 / (1.0 + math.Exp(-(float64(n) - sourceCountInflectionN))))
}

// DiversityFactor returns a step function: 0.5 for one source class,
// 1.0 for ≥2 distinct classes. The ADR-0019 motivation is that
// CEX + DEX agreement is more trustworthy than three CEXs (which
// could share a common upstream feed bug).
//
// classCount = 0 returns 0 (no signal); ≥2 returns 1.0; 1 returns
// 0.5.
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

// LiquidityFactor maps USD bucket volume to a confidence factor on
// a log-saturating shape. Below $1K → ~0; above $1M → ~1.0 (the
// ceiling was $100K until 2026-07-25 — see [liquidityCeilingUSD] for
// the measurement that moved it). The boundary value is computed as a
// log-interpolation between the floor and ceiling.
//
// A NEGATIVE volume is the [LiquidityUnmeasured] sentinel and returns
// the neutral [LiquidityUnmeasuredFactor] (COR-14). Zero and any
// measured volume at or below the floor still return 0: "we looked and
// there is almost nothing here" is a real, confidence-destroying
// signal — it is the leg that lets the Phase 2 freeze fire on a thin
// single-source window — whereas "we never valued this pair" is not a
// finding about the pair at all. Collapsing the two zeroed the whole
// geometric mean for every pair this index cannot price in USD (8 of
// the 12 pairs in the aggregator's defaultPairs() are non-USD-quoted),
// which both served a permanently meaningless confidence and pinned
// the freeze's confidence leg true, degrading its 3-signal AND to two.
//
// NaN returns 0 — that is a caller bug, not a sentinel.
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

// CrossOracleFactor maps cross-oracle divergence (% absolute
// deviation from the cross-reference median) to a confidence
// factor. Piecewise: full credit (1.0) within
// [crossOracleTolerancePct]; exponential decay beyond. 0% → 1.0;
// 1% → 1.0; 5% → ~0.5; 10% → ~0.21; +∞ → 0.
//
// The "no cross-oracle data available" case is signalled by passing
// a negative divergence — the function returns the documented
// neutral value 0.7 (per the ADR-0019 worked example) so the
// geometric mean isn't dragged down on assets without external
// references.
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

// TriangulationAgreementFactor maps the % absolute deviation between
// a pair's direct price and the composite price implied by a
// configured triangulation chain (XLM/EUR direct vs XLM/USD × USD/EUR)
// to a confidence factor. Same piecewise shape as [CrossOracleFactor]:
// full credit (1.0) within [triangulationTolerancePct]; exponential
// decay beyond, halving every [triangulationHalfLifePct]. 0% → 1.0;
// 2% → 1.0; 6% → ~0.5; 20% → ~0.05; +∞ → 0.
//
// Both directions are load-bearing. Agreement is corroboration — a
// thin, single-venue pair whose price is reproduced by an independent
// route through deep markets has earned some credit, and as an extra
// factor in the normalised geometric mean it lifts the score. Large
// divergence is a MANIPULATION SIGNAL, not merely missing information:
// one of the two prices is wrong, and on the pairs this is deployed
// for (87.5% single-source minutes on XLM/EUR, 100% on XLM/GBP as
// measured 2026-07-25) the thin direct print is the likelier suspect.
// So it decays hard rather than reverting to neutral.
//
// The "no composite available" case is signalled by passing a negative
// divergence and returns [triangulationNeutralFactor]; [Compute] also
// zeroes this factor's weight in that case, so an un-triangulated pair
// scores exactly as it did before this factor existed.
//
// NaN returns 0 — a caller bug, not a sentinel (same as every other
// factor here).
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

// BaselineQualityFactor maps how much history backs a per-asset
// baseline, in DAYS-EQUIVALENT of 1-minute buckets, to a confidence
// factor. 0 → 0.5 (bootstrap penalty per ADR-0019 §"Bootstrap
// policy"); 30 → 1.0; linear in between.
//
// The input is sample density, not calendar age (COR-14) — see
// [Inputs.BaselineAgeDays]. A sparsely-traded pair therefore sits low
// on this factor indefinitely, which is the intended reading: its
// median/MAD rest on few observations however long it has existed.
//
// Negative is the caller's explicit "no baseline yet" sentinel and
// returns 0.5 — the same bootstrap value as zero history, because
// "not trained" and "barely trained" deserve the same treatment. NaN
// returns 0.5 for the same reason rather than poisoning the
// geometric mean. (This used to be justified as tolerating a
// "clock-skew-induced negative age"; no wall clock is involved in
// this input at all.)
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

// clamp01 returns x clamped to [0, 1]. Defensive — every factor
// caller passes its output through this so a math edge case can't
// poison the geometric mean.
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
