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
		Confidence:  0.05, // < DefaultPhase2ConfidenceMaxFreeze
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
	// confidence == the threshold — strictly less-than required.
	// Deliberately expressed against the constant rather than a literal:
	// this test is about the STRICTNESS of the comparison, not about
	// whatever value the freeze is currently calibrated to, so a
	// recalibration must not be able to silently invalidate it. (It
	// nearly did — this read `Confidence: 0.10` when the threshold moved
	// from 0.10 to 0.45 on 2026-07-25.)
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: DefaultPhase2ConfidenceMaxFreeze, ZScore: 8.0, SourceCount: 1,
	}, defaultThresh()) {
		t.Errorf("confidence==%v boundary should NOT freeze (strictly <)",
			DefaultPhase2ConfidenceMaxFreeze)
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

// TestPhase2FreezeFires_CalibratedToADRZBand pins the 2026-07-25
// operator decision: DefaultPhase2ConfidenceMaxFreeze = 0.45 exists so
// the freeze actually fires in the neighbourhood of ADR-0019's stated
// `z > 5`, across every population the aggregator serves.
//
// Why this test rather than a comment. Confidence is a weighted
// geometric mean, so it decays gently in z and the freeze's real
// trigger point is an EMERGENT property of (threshold x combiner x
// factor set) — not something any one of them states. At the previous
// 0.10 the emergent trigger was z ~= 15, roughly a 30% move in one
// 1-minute bucket for XLM, i.e. the control was decorative while every
// individual piece still looked correct in isolation. Nothing failed
// when that happened. This test is what fails.
//
// It asserts a BAND (no freeze at z <= 5, freeze by z = 6) rather than
// an exact crossing point, so ordinary combiner tuning stays free while
// a change that pushes the trigger back out to z ~= 15 — or pulls it in
// to z ~= 2 — breaks the build. Both directions are real hazards: a
// freeze that never fires serves manipulated prices, and one that fires
// too readily serves stale LKG prices (its own money bug, MNY-22).
//
// The three populations are the ones that actually differ:
//   - mature + measured liquidity: the 4 USD-quoted default pairs
//   - sparse baseline: any newly-tracked pair (bootstrap-capped)
//   - unmeasured liquidity: the 8 non-USD-quoted default pairs, which
//     before COR-14 had confidence pinned to 0 and so froze at z > 0
func TestPhase2FreezeFires_CalibratedToADRZBand(t *testing.T) {
	base := func(z, ageDays, liquidity float64) float64 {
		return confidence.Compute(confidence.Inputs{
			ZScore:                   z,
			SourceCount:              1,
			SourceClassCount:         1,
			LiquidityUSD:             liquidity,
			CrossOracleDivergencePct: -1,
			BaselineAgeDays:          ageDays,
		}, confidence.DefaultWeights()).Confidence
	}

	for _, pop := range []struct {
		name      string
		ageDays   float64
		liquidity float64
	}{
		{"mature baseline, measured liquidity", 200, 12_000},
		{"sparse baseline (bootstrap-capped)", 3, 12_000},
		{"unmeasured liquidity (non-USD-quoted pair)", 200, confidence.LiquidityUnmeasured},
	} {
		t.Run(pop.name, func(t *testing.T) {
			// At the ADR's own threshold value the freeze must NOT yet
			// fire — z > 5 is strict, so z == 5 is a non-event.
			if got := base(5.0, pop.ageDays, pop.liquidity); phase2FreezeFires(
				confidenceWithSourceCount{Confidence: got, ZScore: 5.0, SourceCount: 1},
				defaultThresh(),
			) {
				t.Errorf("froze at z=5.0 (confidence %.4f) — the trigger has drifted BELOW "+
					"ADR-0019's z>5; freezing serves a stale LKG price, so an over-eager "+
					"freeze is a money bug in its own right", got)
			}
			// By z=6 it must have fired. If this fails the freeze has
			// drifted back toward the dormant z~=15 regime that the 0.45
			// calibration exists to correct.
			if got := base(6.0, pop.ageDays, pop.liquidity); !phase2FreezeFires(
				confidenceWithSourceCount{Confidence: got, ZScore: 6.0, SourceCount: 1},
				defaultThresh(),
			) {
				t.Errorf("did NOT freeze at z=6.0 (confidence %.4f, threshold %v) — the "+
					"3-signal AND is not reachable near ADR-0019's z>5 and the phase-2 "+
					"freeze is effectively dormant again",
					got, DefaultPhase2ConfidenceMaxFreeze)
			}
		})
	}
}
