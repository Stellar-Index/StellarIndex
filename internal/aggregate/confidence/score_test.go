package confidence_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
)

// healthyInputs are the ADR-0019 worked-example values (z=0.3, 6 sources, 2 classes, $250K, 0.4% divergence, 187 days), a regression anchor.
func healthyInputs() confidence.Inputs {
	return confidence.Inputs{
		ZScore:                   0.3,
		SourceCount:              6,
		SourceClassCount:         2,
		LiquidityUSD:             250_000,
		CrossOracleDivergencePct: 0.4,
		BaselineAgeDays:          187,
	}
}

// ordinaryBucketInputs is a typical production bucket with no cross-oracle
// reference (-1 sentinel, neutral factor 0.7).
func ordinaryBucketInputs() confidence.Inputs {
	return confidence.Inputs{
		ZScore:                   1.0,
		SourceCount:              4,
		SourceClassCount:         2,
		LiquidityUSD:             50_000,
		CrossOracleDivergencePct: -1,
		BaselineAgeDays:          200,
	}
}

// TestCompute_HealthyAnchor: the ADR's "about 0.92" is a shape, not an anchor; a fully healthy bucket sits in [0.85, 1.0] and
// this pins that range so shape edits are deliberate.
func TestCompute_HealthyAnchor(t *testing.T) {
	got := confidence.Compute(healthyInputs(), confidence.DefaultWeights())
	if got.Confidence < 0.85 {
		t.Errorf("healthy confidence = %v, want >= 0.85 (high under all factors green)", got.Confidence)
	}
	// Sanity: every per-factor decomposition value is in [0, 1].
	for name, f := range map[string]float64{
		"z":         got.Factors.ZScore,
		"src":       got.Factors.SourceCount,
		"diversity": got.Factors.Diversity,
		"liquidity": got.Factors.Liquidity,
		"xoracle":   got.Factors.CrossOracle,
		"baseline":  got.Factors.BaselineQuality,
	} {
		if f < 0 || f > 1 {
			t.Errorf("factor %q = %v, want in [0, 1]", name, f)
		}
	}
}

// TestCompute_DominatingFactor: one low factor (single source, ~0.12) among healthy ones drops the mean materially but mildly
// (~0.69); raise Weights.SourceCount for a sharper drop. Healthy ~0.92 falls below 0.75.
func TestCompute_DominatingFactor(t *testing.T) {
	healthy := confidence.Compute(healthyInputs(), confidence.DefaultWeights())

	in := healthyInputs()
	in.SourceCount = 1 // factor ≈ 0.12
	degraded := confidence.Compute(in, confidence.DefaultWeights())

	if degraded.Confidence > 0.75 {
		t.Errorf("single-source confidence = %v, want < 0.75 (one low factor must drop the score below the healthy baseline)", degraded.Confidence)
	}
	if healthy.Confidence-degraded.Confidence < 0.15 {
		t.Errorf("healthy %v vs single-source %v: delta too small (%.3f), want > 0.15",
			healthy.Confidence, degraded.Confidence, healthy.Confidence-degraded.Confidence)
	}
}

// TestCompute_AnomalyKillsScore — z >> 5 (a real anomaly) cuts the
// score sharply.
func TestCompute_AnomalyKillsScore(t *testing.T) {
	in := healthyInputs()
	in.ZScore = 8.0
	got := confidence.Compute(in, confidence.DefaultWeights())
	if got.Confidence > 0.7 {
		t.Errorf("z=8 confidence = %v, want < 0.7", got.Confidence)
	}
}

// TestCompute_DegenerateInputsScoreZeroNotNaN: a brand-new asset with no baseline and below-floor liquidity, and extreme
// inputs driving every factor to 0, both score a finite 0 (geometric mean), never NaN from log(0).
func TestCompute_DegenerateInputsScoreZeroNotNaN(t *testing.T) {
	tests := []struct {
		name string
		in   confidence.Inputs
	}{
		{"full bootstrap", confidence.Inputs{
			SourceCount:              1,
			SourceClassCount:         1,
			LiquidityUSD:             500, // below floor
			CrossOracleDivergencePct: -1,  // no data
			BaselineAgeDays:          -1,  // no baseline
		}},
		{"extreme inputs", confidence.Inputs{
			ZScore:                   1e9,
			CrossOracleDivergencePct: 1e9,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := confidence.Compute(tt.in, confidence.DefaultWeights())
			if math.IsNaN(got.Confidence) || math.IsInf(got.Confidence, 0) {
				t.Fatalf("confidence not finite: %v", got.Confidence)
			}
			if got.Confidence != 0 {
				t.Errorf("confidence = %v, want 0", got.Confidence)
			}
		})
	}
}

// TestCompute_AllZeroWeights — degenerate edge case: every weight
// is 0. Total weight is 0; we return a neutral 0.5 plus the
// per-factor decomposition for diagnostics.
func TestCompute_AllZeroWeights(t *testing.T) {
	got := confidence.Compute(healthyInputs(), confidence.Weights{})
	if got.Confidence != 0.5 {
		t.Errorf("zero-weights confidence = %v, want 0.5", got.Confidence)
	}
	if got.Factors.ZScore == 0 {
		t.Error("Factors should still be populated even with zero weights")
	}
}

// TestCompute_WeightingChangesScore — bumping one factor's weight
// shifts the combined score's sensitivity to that factor.
func TestCompute_WeightingChangesScore(t *testing.T) {
	in := healthyInputs()
	in.SourceCount = 2 // factor ≈ 0.27 (low)

	defaults := confidence.Compute(in, confidence.DefaultWeights())

	// Heavy weight on source_count → low SourceCount drags score
	// further toward zero than it would under default weights.
	w := confidence.DefaultWeights()
	w.SourceCount = 5.0
	heavy := confidence.Compute(in, w)

	if heavy.Confidence >= defaults.Confidence {
		t.Errorf("heavy SourceCount weight should drop confidence further; default=%v heavy=%v",
			defaults.Confidence, heavy.Confidence)
	}
}

// ─── Bootstrap cap (ADR-0019 §"Bootstrap policy") ───────────────

// TestCompute_BootstrapCapPreservesLowConfidence: when a bootstrapping asset would naturally score below the cap,
// the cap is a ceiling not a floor, so the natural low score must come through.
func TestCompute_BootstrapCapPreservesLowConfidence(t *testing.T) {
	in := healthyInputs()
	in.BaselineAgeDays = 5 // bootstrap
	in.SourceCount = 1     // pulls confidence way down
	in.LiquidityUSD = 100  // below floor → factor 0
	got := confidence.Compute(in, confidence.DefaultWeights())
	if got.Confidence > confidence.BootstrapConfidenceCap {
		t.Errorf("bootstrap with bad signals: confidence = %v, expected <= %v (cap)",
			got.Confidence, confidence.BootstrapConfidenceCap)
	}
	// AND the natural score should be allowed through (not raised
	// to the cap). Liquidity=0 makes the geomean 0.
	if got.Confidence != 0 {
		t.Errorf("natural-low score should pass; got %v, want 0", got.Confidence)
	}
}

// ─── The combiner is the
// NORMALISED weighted geometric mean ───────────────────────────────

// TestCompute_IsNormalisedGeometricMeanNotBareProduct: code, not the ADR's truncated formula, is authoritative. A bare product
// differs by ~0.44 and would silently change every published confidence and when assets freeze.
// Inputs are an ordinary bucket: mild z, 4 sources over 2 classes, $50K liquidity, no cross-oracle reference, mature baseline.
func TestCompute_IsNormalisedGeometricMeanNotBareProduct(t *testing.T) {
	in := ordinaryBucketInputs()
	got := confidence.Compute(in, confidence.DefaultWeights())

	f := got.Factors
	product := f.ZScore * f.SourceCount * f.Diversity *
		f.Liquidity * f.CrossOracle * f.BaselineQuality

	// 1. Definitional: the served value is the 6th root of the
	//    product (all weights 1.0 → sum(weights) = 6).
	wantNormalised := math.Pow(product, 1.0/6.0)
	if math.Abs(got.Confidence-wantNormalised) > 1e-12 {
		t.Errorf("Compute = %.17f, want prod^(1/6) = %.17f (delta %g)",
			got.Confidence, wantNormalised, got.Confidence-wantNormalised)
	}

	// 2. Pin a concrete number: with the ceiling at $1M rather than $100K the $50K liquidity factor is ln(50)/ln(1000) = 0.56632,
	// not ln(50)/ln(100) = 0.84949, which is the whole change.
	const wantConfidence = 0.81103334478800171 // prod = 0.28459827331540849
	if math.Abs(got.Confidence-wantConfidence) > 1e-9 {
		t.Errorf("Compute = %.17f, want %.17f", got.Confidence, wantConfidence)
	}

	// 3. The bare product ADR-0019's formula block shows is a
	//    materially different number — this is the drift's size.
	if math.Abs(got.Confidence-product) < 0.4 {
		t.Errorf("normalised (%v) and bare product (%v) are indistinguishable here; "+
			"the test can no longer tell the two combiners apart", got.Confidence, product)
	}
}

// TestCompute_WeightsAreRelativeNotAbsolute: scaling every weight must not move the score, which the `^ (1 / sum(weights))`
// exponent guarantees. A bare product would square it (0.868 to 0.753) and push buckets toward the freeze threshold.
func TestCompute_WeightsAreRelativeNotAbsolute(t *testing.T) {
	in := ordinaryBucketInputs()
	base := confidence.Compute(in, confidence.DefaultWeights())

	doubled := confidence.DefaultWeights()
	doubled.ZScore *= 2
	doubled.SourceCount *= 2
	doubled.Diversity *= 2
	doubled.Liquidity *= 2
	doubled.CrossOracle *= 2
	doubled.BaselineQuality *= 2
	scaled := confidence.Compute(in, doubled)

	if math.Abs(base.Confidence-scaled.Confidence) > 1e-12 {
		t.Errorf("uniformly doubling weights changed confidence: %.17f → %.17f; "+
			"weights must be relative influence, not an absolute scale",
			base.Confidence, scaled.Confidence)
	}
}

// TestCompute_NoCrossOracleDataStaysNeutral: 0.7 is the neutral cross-oracle value; the normalised combiner costs a healthy
// bucket ~6 points (1.0 to 0.941), whereas a bare product would cap every such bucket at 0.7.
func TestCompute_NoCrossOracleDataStaysNeutral(t *testing.T) {
	in := confidence.Inputs{
		ZScore:                   0,
		SourceCount:              20,
		SourceClassCount:         2,
		LiquidityUSD:             1_000_000,
		CrossOracleDivergencePct: -1, // no data
		BaselineAgeDays:          365,
	}
	got := confidence.Compute(in, confidence.DefaultWeights())

	if got.Factors.CrossOracle != 0.7 {
		t.Fatalf("CrossOracle factor = %v, want the documented neutral 0.7", got.Factors.CrossOracle)
	}
	if got.Factors.CrossOracleChecked {
		t.Error("CrossOracleChecked = true on the no-data sentinel")
	}
	// 0.7^(1/6) ≈ 0.9423, dragged fractionally by the sub-1.0 z and
	// source factors → 0.9412. Pinning >= 0.90 keeps this a statement
	// about neutrality (and fails at 0.7 under a bare product).
	if got.Confidence < 0.90 {
		t.Errorf("no-cross-oracle confidence = %v, want >= 0.90 — the 0.7 "+
			"no-data value must stay neutral, not cap the score", got.Confidence)
	}
	const wantConfidence = 0.94123253454368350
	if math.Abs(got.Confidence-wantConfidence) > 1e-9 {
		t.Errorf("Compute = %.17f, want %.17f", got.Confidence, wantConfidence)
	}
}

// TestDefaultWeights — sanity that the ADR-0019 factors are unweighted
// and that the one post-ADR factor carries its documented discount.
func TestDefaultWeights(t *testing.T) {
	w := confidence.DefaultWeights()
	for name, v := range map[string]float64{
		"z":         w.ZScore,
		"src":       w.SourceCount,
		"diversity": w.Diversity,
		"liquidity": w.Liquidity,
		"xoracle":   w.CrossOracle,
		"baseline":  w.BaselineQuality,
	} {
		if v != 1.0 {
			t.Errorf("DefaultWeights.%s = %v, want 1.0", name, v)
		}
	}
	// Half weight IS the "a derived path is not an independent venue"
	// discount. Raising it to 1.0 would let a composite
	// corroborate as strongly as an independent external reference.
	if w.TriangulationAgreement != 0.5 {
		t.Errorf("DefaultWeights.TriangulationAgreement = %v, want 0.5 — the "+
			"derived-evidence discount lives in the weight", w.TriangulationAgreement)
	}
}

// TestCompute_CrossOracleAgreementDecomposition: the served checked flag and agreement count separate "neutral because unverified"
// from "neutral because mildly diverging"; the score itself ignores the count.
func TestCompute_CrossOracleAgreementDecomposition(t *testing.T) {
	cases := []struct {
		name          string
		divergencePct float64
		agreement     int
		wantChecked   bool
		wantAgreement int
	}{
		{
			name:          "checked with corroboration",
			divergencePct: 0.4,
			agreement:     4,
			wantChecked:   true,
			wantAgreement: 4,
		},
		{
			name:          "checked, zero divergence, all agree",
			divergencePct: 0,
			agreement:     7,
			wantChecked:   true,
			wantAgreement: 7,
		},
		{
			// Real data can show zero corroborators (every reference
			// responded but none within threshold) — checked stays
			// true, agreement 0 is a genuine "nobody agrees" verdict.
			name:          "checked with zero agreement",
			divergencePct: 12.0,
			agreement:     0,
			wantChecked:   true,
			wantAgreement: 0,
		},
		{
			// The -1 sentinels: unchecked. Agreement must serve as 0
			// (not -1) so the wire never carries a negative count.
			name:          "unchecked sentinels",
			divergencePct: -1,
			agreement:     -1,
			wantChecked:   false,
			wantAgreement: 0,
		},
		{
			// A caller bug pairing "no divergence data" with a
			// positive agreement count must not leak the count:
			// unchecked forces agreement to 0.
			name:          "unchecked suppresses stray agreement",
			divergencePct: -1,
			agreement:     3,
			wantChecked:   false,
			wantAgreement: 0,
		},
		{
			// NaN divergence is the defensive-zero factor branch —
			// still reads as unchecked.
			name:          "NaN divergence reads unchecked",
			divergencePct: math.NaN(),
			agreement:     2,
			wantChecked:   false,
			wantAgreement: 0,
		},
		{
			// Negative agreement with real divergence data clamps to
			// 0 rather than serving a negative count.
			name:          "negative agreement clamps to zero",
			divergencePct: 0.5,
			agreement:     -5,
			wantChecked:   true,
			wantAgreement: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := healthyInputs()
			in.CrossOracleDivergencePct = tc.divergencePct
			in.CrossOracleAgreementCount = tc.agreement
			got := confidence.Compute(in, confidence.DefaultWeights())
			if got.Factors.CrossOracleChecked != tc.wantChecked {
				t.Errorf("CrossOracleChecked = %v, want %v", got.Factors.CrossOracleChecked, tc.wantChecked)
			}
			if got.Factors.CrossOracleAgreement != tc.wantAgreement {
				t.Errorf("CrossOracleAgreement = %d, want %d", got.Factors.CrossOracleAgreement, tc.wantAgreement)
			}
		})
	}
}

// TestCompute_AgreementCountDoesNotChangeScore: the agreement count is transparency-only (ADR-0019; the combiner uses
// divergence-from-median), so computations differing only in it must score identically.
func TestCompute_AgreementCountDoesNotChangeScore(t *testing.T) {
	a := healthyInputs()
	a.CrossOracleAgreementCount = 0
	b := healthyInputs()
	b.CrossOracleAgreementCount = 6
	sa := confidence.Compute(a, confidence.DefaultWeights())
	sb := confidence.Compute(b, confidence.DefaultWeights())
	if sa.Confidence != sb.Confidence {
		t.Errorf("agreement count changed the combined score: %v vs %v — it must be transparency-only",
			sa.Confidence, sb.Confidence)
	}
}

// TestCompute_UnmeasuredLiquidityDoesNotZeroTheScore: a pair not valued in USD must score on the factors that WERE measured;
// passing 0 instead of the sentinel zeroed the mean and pinned the freeze's `confidence < 0.10` leg true.
func TestCompute_UnmeasuredLiquidityDoesNotZeroTheScore(t *testing.T) {
	in := healthyInputs()
	in.LiquidityUSD = confidence.LiquidityUnmeasured
	got := confidence.Compute(in, confidence.DefaultWeights())

	if got.Factors.Liquidity != confidence.LiquidityUnmeasuredFactor {
		t.Fatalf("Liquidity factor = %v, want the neutral %v",
			got.Factors.Liquidity, confidence.LiquidityUnmeasuredFactor)
	}
	if got.Confidence <= 0 {
		t.Fatalf("unmeasured-liquidity confidence = %v, want > 0", got.Confidence)
	}
	// The score must equal a mid-band MEASURED liquidity score, so the neutral factor is the only difference. The collision volume is the
	// curve's geometric midpoint (~$31,622.78); LiquidityUnmeasuredFactor stays 0.5 rather than tracking the curve (see its doc).
	mid := healthyInputs()
	mid.LiquidityUSD = math.Sqrt(1_000.0 * 1_000_000.0) // log-midpoint → factor 0.5
	wantSame := confidence.Compute(mid, confidence.DefaultWeights())
	if math.Abs(got.Confidence-wantSame.Confidence) > 1e-9 {
		t.Errorf("unmeasured confidence = %v, want %v (the neutral factor is 0.5, the same as a $%.0f measured bucket)",
			got.Confidence, wantSame.Confidence, mid.LiquidityUSD)
	}
	// A bucket at min_usd_volume must be distinguishable from "unmeasured" by value alone; the flag stays the contract.
	if confidence.LiquidityFactor(10_000) == confidence.LiquidityUnmeasuredFactor {
		t.Error("LiquidityFactor(min_usd_volume) still collides with the unmeasured neutral")
	}

	// And the discrimination the factor exists for is intact: a
	// MEASURED sub-floor bucket still craters the score to 0.
	thin := healthyInputs()
	thin.LiquidityUSD = 500
	if craterd := confidence.Compute(thin, confidence.DefaultWeights()); craterd.Confidence != 0 {
		t.Errorf("measured sub-$1K bucket scored %v, want 0 — the thin-liquidity signal must stay live",
			craterd.Confidence)
	}
}

// ─── Triangulation (composite) corroboration ──────────────────────

// TestCompute_TriangulationUncheckedIsInert: a pair with no triangulation chain must score bit-for-bit as without the factor.
// A neutral constant would change the 1/sum(weights) exponent and re-score every pair; only a zero WEIGHT is a true no-op,
// so the served value must equal the six-factor mean computed independently here.
func TestCompute_TriangulationUncheckedIsInert(t *testing.T) {
	in := healthyInputs() // TriangulationChecked is false (zero value)
	got := confidence.Compute(in, confidence.DefaultWeights())

	if got.Factors.TriangulationChecked {
		t.Error("TriangulationChecked = true with no composite supplied")
	}
	f := got.Factors
	sixFactorMean := math.Pow(
		f.ZScore*f.SourceCount*f.Diversity*f.Liquidity*f.CrossOracle*f.BaselineQuality,
		1.0/6.0)
	if math.Abs(got.Confidence-sixFactorMean) > 1e-12 {
		t.Errorf("unchecked triangulation moved the score: Compute = %.17f, "+
			"six-factor mean = %.17f (delta %g) — an unchecked corroboration "+
			"factor must not re-score pairs that have no composite",
			got.Confidence, sixFactorMean, got.Confidence-sixFactorMean)
	}
}

// TestCompute_TriangulationZeroValueIsNotAgreement: the float's zero value reads as perfect agreement, so only the explicit
// TriangulationChecked flag may turn the factor on (fail closed).
func TestCompute_TriangulationZeroValueIsNotAgreement(t *testing.T) {
	in := healthyInputs()
	in.TriangulationDivergencePct = 0 // as if never set
	got := confidence.Compute(in, confidence.DefaultWeights())
	if got.Factors.TriangulationChecked {
		t.Fatal("a zero divergence with TriangulationChecked=false read as CHECKED — " +
			"omitting the field must never award corroboration credit")
	}

	checked := healthyInputs()
	checked.TriangulationChecked = true
	checked.TriangulationDivergencePct = 0
	if same := confidence.Compute(checked, confidence.DefaultWeights()); same.Confidence == got.Confidence {
		t.Error("checked and unchecked scored identically — the flag has no effect")
	}
}

// TestCompute_TriangulationAgreementRaisesAndDisagreementLowers pins both directions: a composite matching the direct price
// raises the score, a contradicting one lowers it hard.
func TestCompute_TriangulationAgreementRaisesAndDisagreementLowers(t *testing.T) {
	unchecked := confidence.Compute(healthyInputs(), confidence.DefaultWeights()).Confidence

	agreeIn := healthyInputs()
	agreeIn.TriangulationChecked = true
	agreeIn.TriangulationDivergencePct = 0.3 // inside tolerance
	agree := confidence.Compute(agreeIn, confidence.DefaultWeights())

	disagreeIn := healthyInputs()
	disagreeIn.TriangulationChecked = true
	disagreeIn.TriangulationDivergencePct = 25 // manipulation-shaped
	disagree := confidence.Compute(disagreeIn, confidence.DefaultWeights())

	if !agree.Factors.TriangulationChecked || !disagree.Factors.TriangulationChecked {
		t.Fatal("TriangulationChecked = false on a supplied comparison")
	}
	if agree.Confidence <= unchecked {
		t.Errorf("agreement did not raise confidence: %v vs unchecked %v",
			agree.Confidence, unchecked)
	}
	if disagree.Confidence >= unchecked {
		t.Errorf("25%% direct-vs-composite divergence did not lower confidence: %v vs unchecked %v",
			disagree.Confidence, unchecked)
	}
	// Size check: the penalty must be big enough to matter, and the credit small enough that corroboration alone
	// cannot lift a bad bucket over a threshold.
	if unchecked-disagree.Confidence < 0.15 {
		t.Errorf("disagreement penalty too small: %v → %v", unchecked, disagree.Confidence)
	}
	if agree.Confidence-unchecked > 0.05 {
		t.Errorf("agreement credit too large: %v → %v; a derived path is not an "+
			"independent venue", unchecked, agree.Confidence)
	}
}

// TestCompute_TriangulationNeverTouchesSourceCount: the freeze's third leg is `source_count <= 1`; a composite reuses our own
// legs and must not count as a second source, or it would disarm the freeze on thin single-venue pairs.
func TestCompute_TriangulationNeverTouchesSourceCount(t *testing.T) {
	single := confidence.Inputs{
		ZScore:                   6,
		SourceCount:              1,
		SourceClassCount:         1,
		LiquidityUSD:             12_000,
		CrossOracleDivergencePct: -1,
		BaselineAgeDays:          200,
	}
	withComposite := single
	withComposite.TriangulationChecked = true
	withComposite.TriangulationDivergencePct = 0

	a := confidence.Compute(single, confidence.DefaultWeights())
	b := confidence.Compute(withComposite, confidence.DefaultWeights())

	if a.Factors.SourceCount != b.Factors.SourceCount {
		t.Errorf("a composite changed the source-count factor: %v → %v",
			a.Factors.SourceCount, b.Factors.SourceCount)
	}
	if b.Factors.SourceCount != confidence.SourceCountFactor(1) {
		t.Errorf("source-count factor = %v, want SourceCountFactor(1) = %v — the "+
			"composite must not be counted as a source",
			b.Factors.SourceCount, confidence.SourceCountFactor(1))
	}
}

// TestCompute_zeroWeightOnZeroFactorIsNotNaN: safeLog(0) is -Inf and -Inf * 0 = NaN, which would collapse the score to 0 beside
// healthy factors. A zero weight REMOVES the factor, and doc.go promises the mean never yields NaN.
func TestCompute_zeroWeightOnZeroFactorIsNotNaN(t *testing.T) {
	w := confidence.DefaultWeights()
	w.Liquidity = 0 // a plausible operator response to unmeasurable pairs

	got := confidence.Compute(confidence.Inputs{
		ZScore:           0.5,
		SourceCount:      4,
		SourceClassCount: 2,
		LiquidityUSD:     0, // -> LiquidityFactor 0
		BaselineAgeDays:  400,
	}, w)

	if math.IsNaN(got.Confidence) {
		t.Fatal("Compute returned NaN — a zero-weighted zero factor must be removed, not poison the product")
	}
	if got.Confidence <= 0 {
		t.Errorf("Confidence = %v, want > 0: the liquidity factor carries weight 0, so it must not affect the score at all", got.Confidence)
	}
}

// ─── The bootstrap gate is a DENSITY threshold ──────────

// TestCompute_BootstrapCapReleasesAtDensityThreshold pins the release point as a number on both sides. A gate at 30.0
// density-days is never satisfied (at most 43,200 buckets in 30 days), so the gate is 28.5 (95%), which still implies 28.5 calendar days.
// Change these numbers only as a deliberate money-path decision.
func TestCompute_BootstrapCapReleasesAtDensityThreshold(t *testing.T) {
	const gate = 28.5

	justUnder := healthyInputs()
	justUnder.BaselineAgeDays = gate - 0.01
	if got := confidence.Compute(justUnder, confidence.DefaultWeights()); got.Confidence != confidence.BootstrapConfidenceCap {
		t.Errorf("at %.2f days-equivalent confidence = %v, want the %v cap",
			justUnder.BaselineAgeDays, got.Confidence, confidence.BootstrapConfidenceCap)
	}

	atGate := healthyInputs()
	atGate.BaselineAgeDays = gate
	got := confidence.Compute(atGate, confidence.DefaultWeights())
	if got.Confidence <= confidence.BootstrapConfidenceCap {
		t.Errorf("at the %.2f-days-equivalent gate confidence = %v, want it above the %v cap",
			gate, got.Confidence, confidence.BootstrapConfidenceCap)
	}
	f := got.Factors
	want := math.Pow(f.ZScore*f.SourceCount*f.Diversity*f.Liquidity*f.CrossOracle*f.BaselineQuality, 1.0/6.0)
	if math.Abs(got.Confidence-want) > 1e-12 {
		t.Errorf("at the gate confidence = %.17f, want the uncapped geometric mean %.17f",
			got.Confidence, want)
	}
}

// TestApplyBootstrapCap_SetsBootstrapCapped: the served decomposition names the ceiling and the density gated on, so a capped 0.5
// is distinguishable from a scored one; the flag is set below the 28.5 gate and on the no-baseline sentinel.
func TestApplyBootstrapCap_SetsBootstrapCapped(t *testing.T) {
	cases := []struct {
		age        float64
		wantCapped bool
	}{
		{-1, true},
		{0, true},
		{5, true},
		{28.49, true},
		{28.5, false},
		{30, false},
	}
	for _, c := range cases {
		in := healthyInputs()
		in.BaselineAgeDays = c.age
		got := confidence.Compute(in, confidence.DefaultWeights())
		if got.Factors.BootstrapCapped != c.wantCapped {
			t.Errorf("age %.2f: bootstrap_capped = %v, want %v", c.age, got.Factors.BootstrapCapped, c.wantCapped)
		}
		if got.Factors.BaselineAgeDays != c.age {
			t.Errorf("age %.2f: baseline_age_days = %v, want the scored density %v", c.age, got.Factors.BaselineAgeDays, c.age)
		}
		if c.wantCapped && got.Confidence != confidence.BootstrapConfidenceCap {
			t.Errorf("age %.2f: capped confidence = %v, want %v", c.age, got.Confidence, confidence.BootstrapConfidenceCap)
		}
		if !c.wantCapped && got.Confidence <= confidence.BootstrapConfidenceCap {
			t.Errorf("age %.2f: uncapped confidence = %v, want above %v", c.age, got.Confidence, confidence.BootstrapConfidenceCap)
		}
	}
}

// TestCompute_NonFiniteBaselineAgeStillEncodes — the Score is cached as
// JSON, and encoding/json rejects NaN. An unknown density applies no
// cap and serves as the negative no-baseline reading.
func TestCompute_NonFiniteBaselineAgeStillEncodes(t *testing.T) {
	in := healthyInputs()
	in.BaselineAgeDays = math.NaN()
	got := confidence.Compute(in, confidence.DefaultWeights())
	if got.Factors.BootstrapCapped {
		t.Error("NaN age: bootstrap_capped = true, but applyBootstrapCap applies no cap on NaN")
	}
	if got.Factors.BaselineAgeDays != -1 {
		t.Errorf("NaN age: baseline_age_days = %v, want -1", got.Factors.BaselineAgeDays)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("Score with NaN baseline age does not encode: %v", err)
	}
}

// TestCompute_BootstrapGateHysteresis walks one pair's density down through the band and back up, carrying gate state
// as the orchestrator does: a released pair holds its score until density falls below 27.0; a capped pair releases only at 28.5.
func TestCompute_BootstrapGateHysteresis(t *testing.T) {
	if confidence.BootstrapReengageDensityDays != 27.0 {
		t.Fatalf("re-engage edge = %v, want 27.0 days-equivalent", confidence.BootstrapReengageDensityDays)
	}
	steps := []struct {
		age        float64
		wantCapped bool
	}{
		{28.49, true}, // never released: the upper gate applies
		{28.5, false}, // clears the upper gate
		{28.0, false}, // inside the band: stays released
		{27.0, false}, // at the lower edge: stays released
		{26.99, true}, // below the band: re-capped
		{28.0, true},  // inside the band from below: stays capped
		{28.49, true}, // still under the upper gate
		{28.5, false}, // released again
		{-1, true},    // a lost baseline re-caps a released pair
	}
	released := false
	for i, s := range steps {
		in := healthyInputs()
		in.BaselineAgeDays = s.age
		in.BootstrapReleased = released
		got := confidence.Compute(in, confidence.DefaultWeights())
		if got.Factors.BootstrapCapped != s.wantCapped {
			t.Errorf("step %d (age %.2f, released %v): bootstrap_capped = %v, want %v",
				i, s.age, released, got.Factors.BootstrapCapped, s.wantCapped)
		}
		if s.wantCapped && got.Confidence != confidence.BootstrapConfidenceCap {
			t.Errorf("step %d (age %.2f): capped confidence = %v, want %v",
				i, s.age, got.Confidence, confidence.BootstrapConfidenceCap)
		}
		if !s.wantCapped && got.Confidence <= confidence.BootstrapConfidenceCap {
			t.Errorf("step %d (age %.2f): released confidence = %v, want above %v",
				i, s.age, got.Confidence, confidence.BootstrapConfidenceCap)
		}
		released = !got.Factors.BootstrapCapped
	}
}
