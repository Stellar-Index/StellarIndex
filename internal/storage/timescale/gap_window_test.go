package timescale

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestComputeGapScanWindow pins the trailing-window arithmetic that
// replaced the [genesis, tip] full-history scan (2026-07-06 IO-
// saturation incident). The detector must scan only a bounded trailing
// window each cycle; deep history is the ADR-0033 completeness
// verdict's domain.
func TestComputeGapScanWindow(t *testing.T) {
	t.Parallel()
	// Realistic pubnet-scale genesis + tip so the SafetyLookback /
	// FirstScanCap constants actually bind (they're 200k / 2M).
	const (
		genesis = int64(50_457_424)
		tip     = int64(62_500_000)
	)
	cases := []struct {
		name          string
		genesis, tip  int64
		prevHighWater int64
		firstRun      bool
		want          int64
	}{
		// Steady state: high-water is one 30-min cycle (~360 ledgers)
		// behind tip → scan ONLY that incremental frontier, not 12M
		// ledgers of history.
		{"steady incremental from high-water", genesis, tip, tip - 360, false, tip - 360},
		// High-water fell far behind (detector down for this target
		// longer than SafetyLookback of tip advance) → cap the catch-up
		// at tip - SafetyLookback; do NOT walk back to the stale mark.
		{"post-downtime capped at SafetyLookback", genesis, tip, tip - 5_000_000, false, tip - GapDetectorSafetyLookback},
		// First-ever run (no persisted high-water) → generous but
		// finite FirstScanCap window.
		{"first run uses FirstScanCap", genesis, tip, 0, true, tip - GapDetectorFirstScanCap},
		// Transient high-water read error (firstRun=false, prevHW=0) →
		// falls back to the bounded SafetyLookback window, NOT the wider
		// FirstScanCap.
		{"read-error fallback is SafetyLookback not FirstScanCap", genesis, tip, 0, false, tip - GapDetectorSafetyLookback},
		// Genesis floor: never scan below the source's first-possible
		// ledger even when the window would reach further back (a young
		// source whose genesis is inside the FirstScanCap window).
		{"clamped up to genesis on first run", tip - 500_000, tip, 0, true, tip - 500_000},
		// Genesis floor also binds the SafetyLookback window.
		{"clamped up to genesis on safety window", tip - 100_000, tip, tip - 100_500, false, tip - 100_000},
		// Unit-test scale: tiny tip clamps cleanly, never negative.
		{"tiny tip clamps to genesis", 2, 1000, 0, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeGapScanWindow(tc.genesis, tc.tip, tc.prevHighWater, tc.firstRun)
			if got != tc.want {
				t.Errorf("computeGapScanWindow(g=%d, tip=%d, hw=%d, first=%v) = %d; want %d",
					tc.genesis, tc.tip, tc.prevHighWater, tc.firstRun, got, tc.want)
			}
			if got < 0 {
				t.Errorf("from must never be negative, got %d", got)
			}
			if got > tc.tip && tc.genesis <= tc.tip {
				t.Errorf("from %d exceeds tip %d for an already-deployed source", got, tc.tip)
			}
		})
	}
}

// TestComputeGapScanWindowBoundsScanCost is the incident regression:
// no matter how stale the high-water, a single cycle never scans more
// than FirstScanCap ledgers back from tip — the property that stops
// the two ~13-min full-history LAG scans that saturated IO.
func TestComputeGapScanWindowBoundsScanCost(t *testing.T) {
	t.Parallel()
	const (
		genesis = int64(50_457_424)
		tip     = int64(62_500_000)
	)
	// Steady/post-error path: window width never exceeds SafetyLookback.
	if from := computeGapScanWindow(genesis, tip, 0, false); tip-from > GapDetectorSafetyLookback {
		t.Errorf("steady-state window width %d exceeds SafetyLookback %d", tip-from, GapDetectorSafetyLookback)
	}
	// First-run path: window width never exceeds FirstScanCap.
	if from := computeGapScanWindow(genesis, tip, 0, true); tip-from > GapDetectorFirstScanCap {
		t.Errorf("first-run window width %d exceeds FirstScanCap %d", tip-from, GapDetectorFirstScanCap)
	}
}

// TestGapScanWindowCoverageMathCoherent proves the coverage-percent
// math stays coherent under windowing: a gap detected INSIDE the
// trailing window still drives gap_free down (so it persists + alerts
// as before), and density is honest because expected is the window
// size (tip - from + 1), not tip - genesis + 1. Mismatching numerator
// (window distinct) against a whole-history denominator would collapse
// density toward zero — exactly the trap the incident fix must avoid.
func TestGapScanWindowCoverageMathCoherent(t *testing.T) {
	t.Parallel()
	const (
		genesis = int64(50_000_000)
		tip     = int64(62_500_000)
	)
	from := computeGapScanWindow(genesis, tip, 0, true) // first-run window
	expected := ExpectedLedgersFor(from, tip)
	if expected != tip-from+1 {
		t.Fatalf("window expected = %d; want %d (tip-from+1)", expected, tip-from+1)
	}

	// Window fully covered except one 120k-ledger interior gap.
	maxGap := int64(120_000)
	distinct := expected - maxGap
	cov := SourceCoverageFromCounts("sep41-transfers", "sep41_transfers",
		distinct, expected, maxGap, 1, time.Now().UTC())

	// Density is window-scoped and coherent (~0.94), NOT collapsed by a
	// whole-history denominator.
	if cov.DensityPct <= 0.9 || cov.DensityPct > 1.0 {
		t.Errorf("window density = %v; want coherent value in (0.9, 1.0]", cov.DensityPct)
	}
	// The in-window gap still drives gap_free below 1.0 — the alerting
	// signal is preserved for recent gaps.
	wantGapFree := 1 - float64(maxGap)/float64(expected)
	if cov.GapFreePct != wantGapFree {
		t.Errorf("gap_free = %v; want %v", cov.GapFreePct, wantGapFree)
	}
	if cov.GapFreePct >= 1.0 {
		t.Errorf("gap_free must drop below 1.0 when a gap is present, got %v", cov.GapFreePct)
	}
}

// TestMarkGapDetectorScanSuccessAdvancesLivenessGauge pins the crux of
// the 2026-07-06 false-`stellarindex_ingest_gap_detector_silent` fix: a
// successful scan stamps the wall-clock last-success gauge the alert
// keys off. The once-per-6h `ok` COUNTER is invisible to `rate()` across
// a restart (its value is 1 → 1, no detectable reset), so liveness must
// ride a reset-proof timestamp gauge instead. This test proves the
// success path advances that gauge.
func TestMarkGapDetectorScanSuccessAdvancesLivenessGauge(t *testing.T) {
	t.Parallel()
	// Unique (source, table) so the assertion is isolated from the real
	// detector targets and from parallel tests sharing the global registry.
	target := GapDetectorTarget{Source: "test-liveness-src", Table: "test_liveness_tbl"}

	now := time.Unix(1_700_000_000, 0)
	markGapDetectorScanSuccess(target, now.Add(-3*time.Second), now)

	got := testutil.ToFloat64(obs.IngestGapDetectorLastSuccessUnix.WithLabelValues(target.Source, target.Table))
	if want := float64(now.Unix()); got != want {
		t.Fatalf("last-success gauge = %v; want %v (now.Unix())", got, want)
	}
	// The ok counter increments alongside, but it is NOT the liveness
	// signal — the alert no longer reads its rate.
	if c := testutil.ToFloat64(obs.IngestGapDetectorRunsTotal.WithLabelValues(target.Source, target.Table, "ok")); c != 1 {
		t.Fatalf("ok counter = %v; want 1", c)
	}

	// A later successful scan advances the stamp forward: staleness
	// (time() - gauge) resets, which is exactly what clears the alert on a
	// healthy scan even after the process (and thus the counter) reset.
	later := now.Add(6 * time.Hour)
	markGapDetectorScanSuccess(target, later.Add(-2*time.Second), later)
	got = testutil.ToFloat64(obs.IngestGapDetectorLastSuccessUnix.WithLabelValues(target.Source, target.Table))
	if want := float64(later.Unix()); got != want {
		t.Fatalf("after second scan, gauge = %v; want %v", got, want)
	}
}
