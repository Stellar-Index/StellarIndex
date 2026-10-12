package orchestrator

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
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
	// Confidence == the threshold: strictly less-than is required. Expressed against the constant, not a literal, so recalibrating the threshold cannot silently invalidate this strictness test.
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

// TestPhase2FreezeFires_ConfidenceConditionIsNotVacuous feeds real [confidence.Compute] output for an unremarkable single-source bucket: it scores 0.530, so confidence does NOT hold and source count alone does not freeze.
// Under the bare product in ADR-0019's formula block it would score 0.0223, every single-source window would satisfy the condition, and the AND would collapse to two signals; changing the combiner without this threshold is not a docs-only change.
func TestPhase2FreezeFires_ConfidenceConditionIsNotVacuous(t *testing.T) {
	singleSourceButOrdinary := confidence.Inputs{
		ZScore:                   0.5,
		SourceCount:              1,
		SourceClassCount:         1,
		LiquidityUSD:             12_000, // just over the $10k publish floor
		CrossOracleDivergencePct: -1,     // no external reference
		BaselineAgeDays:          maxBaselineAgeDays,
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

// TestPhase2FreezeFires_CalibratedToADRZBand pins DefaultPhase2ConfidenceMaxFreeze = 0.45 so the freeze fires near ADR-0019's z > 5. The real trigger is emergent from threshold x combiner x factors; at 0.10 it was z ~ 15 (a ~30% one-minute XLM move) and nothing failed.
// It asserts a band (no freeze at z <= 5, freeze by z = 6) so combiner tuning stays free but drift toward z ~ 15 (manipulated prices served) or z ~ 2 (stale LKG served) breaks the build.
// Populations: mature with measured liquidity (4 USD-quoted pairs), sparse baseline (bootstrap-capped), and unmeasured liquidity (8 non-USD pairs, which without the sentinel freeze at z > 0).
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
		{"mature baseline, measured liquidity", maxBaselineAgeDays, 12_000},
		{"sparse baseline (bootstrap-capped)", 3, 12_000},
		{"unmeasured liquidity (non-USD-quoted pair)", maxBaselineAgeDays, confidence.LiquidityUnmeasured},
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

// TestLogFreezeTransition_HeldUnscored_IsCounted: a hold expiry on an unscored bucket ([freeze.TransitionHeldUnscored]) must increment a dedicated counter, so a sustained run (stuck scorer) is not hidden in the Debug-only default arm.
func TestLogFreezeTransition_HeldUnscored_IsCounted(t *testing.T) {
	o := &Orchestrator{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	pair := xlmUsdtPair(t)

	before := testutil.ToFloat64(obs.AnomalyFreezeHeldUnscoredTotal)

	o.logFreezeTransition(pair, time.Hour, anomaly.Decision{}, freeze.Outcome{
		Transition: freeze.TransitionHeldUnscored,
		State:      freeze.State{ExtensionsUsed: 1},
	})

	after := testutil.ToFloat64(obs.AnomalyFreezeHeldUnscoredTotal)
	if got := after - before; got != 1 {
		t.Errorf("AnomalyFreezeHeldUnscoredTotal increment = %v, want 1", got)
	}
}
