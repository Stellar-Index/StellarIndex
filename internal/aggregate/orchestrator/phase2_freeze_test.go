package orchestrator

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
)

// defaultThresh returns the package-default thresholds via the
// zero-value's `withDefaults` merge — keeps the tests aligned with
// "operator hasn't overridden anything" production behaviour.
func defaultThresh() Phase2Thresholds { return Phase2Thresholds{} }

// TestPhase2FreezeFires_AllThree — the 3-signal AND fires when
// every input crosses its threshold.
func TestPhase2FreezeFires_AllThree(t *testing.T) {
	got := phase2FreezeFires(confidenceWithSourceCount{
		Confidence:  0.05, // < 0.10
		ZScore:      8.0,  // > 5.0
		SourceCount: 1,    // <= 1
	}, defaultThresh())
	if !got {
		t.Error("phase2FreezeFires returned false on a clean 3-signal hit")
	}
}

// TestPhase2FreezeFires_MissingOneSignal — any single signal
// failing the threshold suppresses the freeze. Walks each of the
// three signals being just-clean while the other two are anomalous.
func TestPhase2FreezeFires_MissingOneSignal(t *testing.T) {
	// Multi-source: even with very low confidence + huge z, having
	// 2 sources keeps the freeze off.
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.05, ZScore: 8.0, SourceCount: 2,
	}, defaultThresh()) {
		t.Error("multi-source bucket should NOT freeze")
	}
	// Sub-threshold z: deviation isn't large enough for a freeze.
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.05, ZScore: 4.5, SourceCount: 1,
	}, defaultThresh()) {
		t.Error("z=4.5 bucket should NOT freeze (below 5.0 threshold)")
	}
	// Healthy confidence: every other signal looks bad but the
	// confidence-score combiner says "trustworthy"; don't freeze.
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.50, ZScore: 8.0, SourceCount: 1,
	}, defaultThresh()) {
		t.Error("confidence=0.50 bucket should NOT freeze")
	}
}

// TestPhase2FreezeFires_BoundaryStrictness — the conditions are
// strictly > / < / <=. Boundary values don't fire.
func TestPhase2FreezeFires_BoundaryStrictness(t *testing.T) {
	// confidence == 0.10 — strictly less-than required.
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.10, ZScore: 8.0, SourceCount: 1,
	}, defaultThresh()) {
		t.Error("confidence==0.10 boundary should NOT freeze (strictly <)")
	}
	// z == 5.0 — strictly greater-than required.
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.05, ZScore: 5.0, SourceCount: 1,
	}, defaultThresh()) {
		t.Error("z==5.0 boundary should NOT freeze (strictly >)")
	}
	// source_count == 1 — the boundary IS included (≤).
	if !phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.05, ZScore: 8.0, SourceCount: 1,
	}, defaultThresh()) {
		t.Error("source_count==1 boundary SHOULD freeze (<=)")
	}
}

// TestPhase2FreezeFires_ZeroSources — no contributing sources is
// the most-pathological case and freezes if the other two signals
// agree.
func TestPhase2FreezeFires_ZeroSources(t *testing.T) {
	if !phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.05, ZScore: 8.0, SourceCount: 0,
	}, defaultThresh()) {
		t.Error("zero-source bucket with other signals firing SHOULD freeze")
	}
}

// TestPhase2FreezeFires_OperatorOverride — operators can tighten
// the gate. Setting ZScoreMinFreeze=10 means a z=8 bucket (which
// fires under defaults) does NOT fire under the override.
func TestPhase2FreezeFires_OperatorOverride(t *testing.T) {
	stricter := Phase2Thresholds{ZScoreMinFreeze: 10.0}
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.05, ZScore: 8.0, SourceCount: 1,
	}, stricter) {
		t.Error("z=8 bucket should NOT freeze when ZScoreMinFreeze=10")
	}
	// Confirm the same bucket DOES freeze under the default.
	if !phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.05, ZScore: 8.0, SourceCount: 1,
	}, defaultThresh()) {
		t.Error("sanity: same bucket should freeze under defaults")
	}
}

// TestPhase2FreezeFires_PartialOverrideMergesDefaults — an operator
// who only sets one field gets the package defaults for the others.
func TestPhase2FreezeFires_PartialOverrideMergesDefaults(t *testing.T) {
	// Override only ConfidenceMaxFreeze — the others should fall
	// back to defaults (z>5.0, sources<=1).
	override := Phase2Thresholds{ConfidenceMaxFreeze: 0.05}
	// At confidence=0.04 (< 0.05), z=8 (> default 5), sources=1 (<= default 1) →
	// fires because all three conditions hold.
	if !phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.04, ZScore: 8.0, SourceCount: 1,
	}, override) {
		t.Error("partial override should merge defaults; this bucket should freeze")
	}
	// At confidence=0.08 (> override threshold), the freeze should NOT fire.
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.08, ZScore: 8.0, SourceCount: 1,
	}, override) {
		t.Error("confidence=0.08 with override threshold 0.05 should NOT freeze")
	}
}

// TestPhase2FreezeFires_ConfidenceConditionIsNotVacuous — R-003
// (audit-2026-07-23, COR-14). The freeze is a THREE-signal AND, and
// the confidence signal only carries information because
// confidence.Compute normalises the weighted geometric mean.
//
// This test feeds real [confidence.Compute] output (not a literal)
// for a single-source bucket that is otherwise unremarkable — a low
// z, one source class, mature baseline, no cross-oracle reference,
// and $12K of window liquidity (just over the production
// min_usd_volume floor). Under the shipped combiner it scores 0.530,
// so the confidence sub-condition does NOT hold and the bucket does
// NOT freeze on source-count alone.
//
// Under the bare product ADR-0019's formula block shows, the same
// bucket scores 0.0223 — below the 0.10 threshold before z is even
// considered, because source_count_factor(1) = 0.119 and
// diversity_factor(1) = 0.5 multiply to 0.06 on their own. Every
// single-source window would satisfy the confidence condition, the
// AND would collapse to two signals, and freezes (serving stale LKG
// prices on /v1/price) would fire far more often. Changing the
// combiner without changing this threshold is therefore NOT a
// documentation-only change — that is what this test guards.
func TestPhase2FreezeFires_ConfidenceConditionIsNotVacuous(t *testing.T) {
	singleSourceButOrdinary := confidence.Inputs{
		ZScore:                   0.5,
		SourceCount:              1,
		SourceClassCount:         1,
		LiquidityUSD:             12_000, // just over the $10k publish floor
		CrossOracleDivergencePct: -1,     // no external reference
		BaselineAgeDays:          200,
	}
	score := confidence.Compute(singleSourceButOrdinary, confidence.DefaultWeights())

	if score.Confidence < DefaultPhase2ConfidenceMaxFreeze {
		t.Fatalf("single-source-but-ordinary bucket scored %v, below the %v freeze "+
			"threshold — the confidence sub-condition is vacuous for single-source "+
			"windows and the 3-signal AND has collapsed to 2 signals",
			score.Confidence, DefaultPhase2ConfidenceMaxFreeze)
	}

	// Same bucket, deeply anomalous z: still no freeze, because the
	// confidence signal has not agreed. (Two of three is not enough —
	// that is the documented ADR-0019 semantic.)
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: score.Confidence, ZScore: 8.0, SourceCount: 1,
	}, defaultThresh()) {
		t.Errorf("bucket with confidence=%v froze on z + source_count alone", score.Confidence)
	}

	// And the signal is live, not dead: drive a factor to zero
	// (sub-$1K liquidity → liquidity_factor 0) and confidence craters
	// to 0, so the same z + source_count DOES freeze.
	crater := singleSourceButOrdinary
	crater.LiquidityUSD = 500
	cratered := confidence.Compute(crater, confidence.DefaultWeights())
	if cratered.Confidence >= DefaultPhase2ConfidenceMaxFreeze {
		t.Fatalf("dust-liquidity bucket scored %v, expected below %v",
			cratered.Confidence, DefaultPhase2ConfidenceMaxFreeze)
	}
	if !phase2FreezeFires(confidenceWithSourceCount{
		Confidence: cratered.Confidence, ZScore: 8.0, SourceCount: 1,
	}, defaultThresh()) {
		t.Error("dust-liquidity + z=8 + single-source should freeze")
	}
}
