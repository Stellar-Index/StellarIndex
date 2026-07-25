package confidence_test

import (
	"math"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
)

// healthyInputs are the ADR-0019 worked-example values:
//
//	z=0.3, sources=6, classes=2, liquidity=$250K, divergence=0.4%,
//	baseline_age=187 days
//
// Expected confidence ≈ 0.92 per the ADR. Used as a regression
// anchor so a future tweak to factor shapes doesn't accidentally
// drift the documented worked example.
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

// TestCompute_HealthyAnchor — the ADR-0019 worked-example inputs
// produce a high confidence under the implemented factor shapes.
//
// The ADR's illustrative figure of "≈ 0.92" describes the
// SHAPE of the response (high but not 1.0) rather than a
// mathematical anchor. With our z/source/etc. factor shapes a
// fully-healthy bucket sits in [0.85, 1.0]; this test pins that
// range so future shape edits surface as deliberate decisions
// rather than silent regressions.
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

// TestCompute_DominatingFactor — ADR-0019: "any one factor near
// zero pulls the whole score down". A single low factor (single-
// source asset, ~0.12) materially drops the combined score.
//
// With 6 equal-weight factors, the geometric mean's "domination"
// is real but mild — a 0.12 factor among five 1.0s gives
// confidence ≈ 0.69, not "near zero". To get a sharper drop the
// operator can raise that factor's weight via `Weights.SourceCount`.
// Verifies the healthy → degraded delta is significant: the
// healthy anchor scores ~0.92, single-source drags below 0.75.
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

// TestCompute_FullBootstrap — a brand-new asset with no baseline,
// no cross-oracle, single source, low liquidity. Score should be
// low but well-defined (no NaN / Inf).
func TestCompute_FullBootstrap(t *testing.T) {
	in := confidence.Inputs{
		ZScore:                   0,
		SourceCount:              1,
		SourceClassCount:         1,
		LiquidityUSD:             500, // below floor
		CrossOracleDivergencePct: -1,  // no data
		BaselineAgeDays:          -1,  // no baseline
	}
	got := confidence.Compute(in, confidence.DefaultWeights())
	if math.IsNaN(got.Confidence) || math.IsInf(got.Confidence, 0) {
		t.Errorf("bootstrap confidence not finite: %v", got.Confidence)
	}
	if got.Confidence < 0 || got.Confidence > 1 {
		t.Errorf("bootstrap confidence outside [0, 1]: %v", got.Confidence)
	}
	// LiquidityFactor returns 0 for below-floor input → geometric
	// mean is 0 (dominating-factor behaviour).
	if got.Confidence != 0 {
		t.Errorf("bootstrap with liquidity=0 should crater to 0, got %v", got.Confidence)
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

// TestCompute_NumericalStability — extreme inputs don't produce
// NaN. Any factor returning exactly 0 + non-zero weight should
// produce a 0 score (not NaN from log(0)).
func TestCompute_NumericalStability(t *testing.T) {
	in := confidence.Inputs{
		ZScore:                   1e9, // → factor ~0
		SourceCount:              0,   // → factor 0
		SourceClassCount:         0,
		LiquidityUSD:             0,   // → factor 0
		CrossOracleDivergencePct: 1e9, // → factor ~0
		BaselineAgeDays:          0,
	}
	got := confidence.Compute(in, confidence.DefaultWeights())
	if math.IsNaN(got.Confidence) || math.IsInf(got.Confidence, 0) {
		t.Errorf("extreme inputs produced %v, want finite", got.Confidence)
	}
	if got.Confidence != 0 {
		t.Errorf("all-zero factors → confidence should be 0, got %v", got.Confidence)
	}
}

// ─── Bootstrap cap (ADR-0019 §"Bootstrap policy") ───────────────

// TestCompute_BootstrapCapsConfidence — an asset with <30d of
// history has confidence hard-capped at 0.5 regardless of how
// healthy every other factor is.
func TestCompute_BootstrapCapsConfidence(t *testing.T) {
	in := healthyInputs()
	in.BaselineAgeDays = 5 // freshly-listed
	got := confidence.Compute(in, confidence.DefaultWeights())
	if got.Confidence > confidence.BootstrapConfidenceCap+1e-9 {
		t.Errorf("bootstrap confidence = %v, want <= %v",
			got.Confidence, confidence.BootstrapConfidenceCap)
	}
}

// TestCompute_BootstrapCapAtZeroAge — exactly 0 days of history
// (just-listed asset) is the most-bootstrap state. Cap fires.
func TestCompute_BootstrapCapAtZeroAge(t *testing.T) {
	in := healthyInputs()
	in.BaselineAgeDays = 0
	got := confidence.Compute(in, confidence.DefaultWeights())
	if got.Confidence > confidence.BootstrapConfidenceCap {
		t.Errorf("zero-age confidence = %v, want capped at %v",
			got.Confidence, confidence.BootstrapConfidenceCap)
	}
}

// TestCompute_BootstrapCapNotAppliedAt30Days — exactly at the
// threshold the cap turns OFF. Healthy bucket reads its full
// confidence.
func TestCompute_BootstrapCapNotAppliedAt30Days(t *testing.T) {
	in := healthyInputs()
	in.BaselineAgeDays = 30 // boundary — strictly < BootstrapDays required
	got := confidence.Compute(in, confidence.DefaultWeights())
	if got.Confidence <= confidence.BootstrapConfidenceCap {
		t.Errorf("at 30d the cap should not apply; got %v <= %v",
			got.Confidence, confidence.BootstrapConfidenceCap)
	}
}

// TestCompute_BootstrapCapPreservesLowConfidence — when an asset
// is in bootstrap AND would naturally score below the cap (e.g.
// single-source low-liquidity), the cap is a CEILING not a floor.
// The natural-low score must still come through.
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

// TestCompute_BootstrapCapWithNoBaselineSentinel — BaselineAgeDays
// = -1 (the "no baseline yet" sentinel) is stricter than bootstrap
// — the cap MUST fire.
func TestCompute_BootstrapCapWithNoBaselineSentinel(t *testing.T) {
	in := healthyInputs()
	in.BaselineAgeDays = -1 // no baseline at all
	got := confidence.Compute(in, confidence.DefaultWeights())
	if got.Confidence > confidence.BootstrapConfidenceCap {
		t.Errorf("no-baseline confidence = %v, want capped at %v",
			got.Confidence, confidence.BootstrapConfidenceCap)
	}
}

// ─── R-003 (audit-2026-07-23, COR-14): the combiner is the
// NORMALISED weighted geometric mean ───────────────────────────────

// TestCompute_IsNormalisedGeometricMeanNotBareProduct pins the
// combiner ADR-0019's formula block omitted the exponent for:
//
//	confidence = prod(factor_i ^ weight_i) ^ (1 / sum(weights))
//
// The ADR's R-003 amendment records the code as authoritative, so
// this is the test that keeps it that way. A future "fix" that makes
// the code match the ADR's *truncated* formula (a bare product) fails
// here with a ~0.44 gap — and would ship a silent, unannounced change
// to every published `confidence` value AND to when assets freeze.
//
// Values below are the shape of an ordinary production bucket: mild
// z, four sources across two classes, $50K liquidity, no cross-oracle
// reference yet (Phase 3 not universally live), mature baseline.
func TestCompute_IsNormalisedGeometricMeanNotBareProduct(t *testing.T) {
	in := confidence.Inputs{
		ZScore:                   1.0,
		SourceCount:              4,
		SourceClassCount:         2,
		LiquidityUSD:             50_000,
		CrossOracleDivergencePct: -1, // no cross-oracle data → neutral 0.7
		BaselineAgeDays:          200,
	}
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

	// 2. Concrete corrected value, so this test pins a number rather
	//    than a tautology against whatever the code happens to do.
	//    Recomputed 2026-07-25 when liquidityCeilingUSD moved $100K →
	//    $1M: the $50K bucket's liquidity factor drops from
	//    ln(50)/ln(100) = 0.84949 to ln(50)/ln(1000) = 0.56632, which is
	//    the whole of the change here.
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

// TestCompute_WeightsAreRelativeNotAbsolute — the property that makes
// the `^ (1 / sum(weights))` exponent load-bearing: ADR-0019 sells
// `[anomaly.weights]` as per-factor *influence*, so scaling every
// weight by a constant must not move the score. Under a bare product
// doubling the weights squares the result (0.868 → 0.753 here), which
// would silently push buckets toward the freeze threshold whenever an
// operator touched the weight block.
func TestCompute_WeightsAreRelativeNotAbsolute(t *testing.T) {
	in := confidence.Inputs{
		ZScore:                   1.0,
		SourceCount:              4,
		SourceClassCount:         2,
		LiquidityUSD:             50_000,
		CrossOracleDivergencePct: -1,
		BaselineAgeDays:          200,
	}
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

// TestCompute_NoCrossOracleDataStaysNeutral — ADR-0019 documents 0.7
// as the NEUTRAL cross-oracle value for "no external reference
// available". Under the normalised combiner that costs a fully-
// healthy bucket ~6 points (1.0 → 0.941); under the bare product the
// same 0.7 would CAP every such bucket at 0.7, i.e. a penalty, not
// neutrality — and today most pairs have no cross-oracle reference.
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
	// discount (2026-07-25). Raising it to 1.0 would let a composite
	// corroborate as strongly as an independent external reference.
	if w.TriangulationAgreement != 0.5 {
		t.Errorf("DefaultWeights.TriangulationAgreement = %v, want 0.5 — the "+
			"derived-evidence discount lives in the weight", w.TriangulationAgreement)
	}
}

// TestCompute_CrossOracleAgreementDecomposition — ADR-0019 Phase 3:
// the served decomposition carries the cross-oracle checked flag +
// agreement count so consumers can distinguish "neutral because
// unverified" from "neutral because mildly diverging" (the CS-087
// DivergenceChecked discipline applied to the confidence surface).
// The combined score itself is unchanged by the agreement count —
// the ADR's cross_oracle_factor input is divergence-from-median.
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

// TestCompute_AgreementCountDoesNotChangeScore — the agreement count
// is transparency-only per ADR-0019 (the combiner's cross-oracle
// input is divergence-from-median). Two computations differing only
// in agreement count must produce identical combined scores.
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

// TestCompute_UnmeasuredLiquidityDoesNotZeroTheScore (COR-14) — a pair
// this index cannot value in USD (every non-USD-quoted pair) must still
// score on the factors that WERE measured. Passing 0 for it instead of
// the sentinel drove the geometric mean to exactly 0, so the served
// confidence carried no information and the Phase 2 freeze's
// `confidence < 0.10` leg was pinned true for those pairs.
//
// Proven red against the pre-fix LiquidityFactor (negative → 0): the
// unmeasured score came back 0, failing both the ">0" and the
// "≈ measured-mid-band" assertions below.
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
	// The score must equal what the same bucket scores with a
	// mid-band MEASURED liquidity — i.e. the neutral factor is the only
	// difference, nothing else silently changed.
	//
	// The collision volume is the curve's GEOMETRIC midpoint, which the
	// 2026-07-25 ceiling change moved from $10,000 (the production
	// min_usd_volume floor — the worst possible place for it) to
	// ≈ $31,622.78. LiquidityUnmeasuredFactor deliberately stayed at
	// 0.5 rather than tracking the curve; see its doc comment for why
	// following it down to 0.333 would have made the 8 non-USD-quoted
	// pairs freeze more readily for a reason that says nothing about
	// them.
	mid := healthyInputs()
	mid.LiquidityUSD = math.Sqrt(1_000.0 * 1_000_000.0) // log-midpoint → factor 0.5
	wantSame := confidence.Compute(mid, confidence.DefaultWeights())
	if math.Abs(got.Confidence-wantSame.Confidence) > 1e-9 {
		t.Errorf("unmeasured confidence = %v, want %v (the neutral factor is 0.5, the same as a $%.0f measured bucket)",
			got.Confidence, wantSame.Confidence, mid.LiquidityUSD)
	}
	// And the collision is no longer at the publish floor: a bucket at
	// min_usd_volume must now be DISTINGUISHABLE from "unmeasured" by
	// value alone (the flag remains the contract, but the coincidence
	// that motivated it is gone).
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

// TestCompute_TriangulationUncheckedIsInert is the load-bearing safety
// property of the whole composite-corroboration feature: a pair with no
// triangulation chain — which is EVERY pair in the index until an
// operator configures one — must score exactly what it scored before
// the factor existed, to the last bit.
//
// A "neutral" constant would not have achieved that. Compute is a
// NORMALISED geometric mean, so adding any seventh factor value changes
// the 1/sum(weights) exponent and therefore re-scores every pair in the
// index — silently moving the Phase 2 freeze's confidence leg for
// pairs that gained nothing. Only zeroing the unchecked factor's WEIGHT
// is a true no-op, and this test pins that: the served value equals the
// six-factor mean computed independently here.
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

// TestCompute_TriangulationZeroValueIsNotAgreement — the fail-closed
// default. `TriangulationDivergencePct` is a float, so its zero value
// is 0.0 = "the composite agrees perfectly"; if that alone flipped the
// factor to checked, every caller that never heard of the field would
// silently collect full corroboration credit. The explicit
// TriangulationChecked flag is what prevents it.
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

// TestCompute_TriangulationAgreementRaisesAndDisagreementLowers pins
// the direction of the signal in both directions, which is the point of
// the factor: a composite that reproduces the direct price is
// corroboration (score up), and one that contradicts it is a
// manipulation signal on one side or the other (score down, hard).
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
	// Size check: the penalty must be big enough to matter (a
	// manipulation signal that moves the score by 0.001 is decoration),
	// and the credit small enough that corroboration alone can't lift a
	// bad bucket over a threshold.
	if unchecked-disagree.Confidence < 0.15 {
		t.Errorf("disagreement penalty too small: %v → %v", unchecked, disagree.Confidence)
	}
	if agree.Confidence-unchecked > 0.05 {
		t.Errorf("agreement credit too large: %v → %v; a derived path is not an "+
			"independent venue", unchecked, agree.Confidence)
	}
}

// TestCompute_TriangulationNeverTouchesSourceCount — ADR-0019's freeze
// is a 3-signal AND whose third leg is `source_count <= 1`. A composite
// re-uses our own legs and pipeline, so it must not count as a second
// source: if it did, configuring a chain would silently disarm the
// freeze on exactly the thin single-venue pairs chains are deployed for.
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
