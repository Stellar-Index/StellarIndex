package confidence_test

import (
	"math"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
)

func near(a, b, tol float64) bool {
	return math.Abs(a-b) < tol
}

// ─── ZScoreFactor ────────────────────────────────────────────────

// TestZScoreFactor_AnchorPoints — the ADR's documented shape
// (1.0 at z=0, ~0.5 at z=5 (the 5σ trigger), ~0 at z=10).
func TestZScoreFactor_AnchorPoints(t *testing.T) {
	cases := []struct {
		z, want, tol float64
	}{
		{z: 0, want: 1.0, tol: 0.01},
		{z: 5, want: 0.5, tol: 0.01},
		{z: 10, want: 0.01, tol: 0.01},
	}
	for _, c := range cases {
		got := confidence.ZScoreFactor(c.z)
		if !near(got, c.want, c.tol) {
			t.Errorf("ZScoreFactor(%v) = %v, want ~%v (±%v)", c.z, got, c.want, c.tol)
		}
	}
}

func TestZScoreFactor_Monotonic(t *testing.T) {
	// Higher z must NEVER produce a higher confidence factor.
	prev := math.Inf(1)
	for z := 0.0; z <= 15; z += 0.5 {
		got := confidence.ZScoreFactor(z)
		if got > prev {
			t.Errorf("ZScoreFactor not monotonic at z=%v: %v > prev %v", z, got, prev)
		}
		prev = got
	}
}

func TestZScoreFactor_GuardsBadInputs(t *testing.T) {
	for _, in := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1.0} {
		got := confidence.ZScoreFactor(in)
		if got != 0 {
			t.Errorf("ZScoreFactor(%v) = %v, want 0 (defensive)", in, got)
		}
	}
}

// ─── SourceCountFactor ────────────────────────────────────────────

func TestSourceCountFactor_AnchorPoints(t *testing.T) {
	cases := []struct {
		n         int
		want, tol float64
	}{
		{n: 1, want: 0.12, tol: 0.05}, // ADR: "single-source caps at ~0.3" — sigmoid gives 0.119
		{n: 3, want: 0.5, tol: 0.01},
		{n: 6, want: 0.95, tol: 0.05}, // ADR: "n≥6 reaches near-1.0"
	}
	for _, c := range cases {
		got := confidence.SourceCountFactor(c.n)
		if !near(got, c.want, c.tol) {
			t.Errorf("SourceCountFactor(%d) = %v, want ~%v (±%v)", c.n, got, c.want, c.tol)
		}
	}
}

func TestSourceCountFactor_GuardsNegative(t *testing.T) {
	if got := confidence.SourceCountFactor(-1); got != 0 {
		t.Errorf("SourceCountFactor(-1) = %v, want 0", got)
	}
}

// ─── DiversityFactor ──────────────────────────────────────────────

func TestDiversityFactor(t *testing.T) {
	cases := []struct {
		classCount int
		want       float64
	}{
		{0, 0},
		{1, 0.5},
		{2, 1.0},
		{5, 1.0}, // any number ≥ 2 is full credit
	}
	for _, c := range cases {
		got := confidence.DiversityFactor(c.classCount)
		if got != c.want {
			t.Errorf("DiversityFactor(%d) = %v, want %v", c.classCount, got, c.want)
		}
	}
}

// ─── LiquidityFactor ──────────────────────────────────────────────

// TestLiquidityFactor_AnchorPoints pins the curve's endpoints and the
// two interior volumes that matter operationally: the production
// `min_usd_volume` publish floor ($10K) and the measured p50 bucket
// volume of the index's deepest pair (BTC/USD, $123,678 as of
// 2026-07-25).
//
// Proven red against the pre-2026-07-25 $100K ceiling: that curve
// returned 1.0 at $100K and at $123,678 — the deepest pair's MEDIAN
// bucket saturated the factor, so it carried no information across the
// whole top half of its own population and $100K of wash volume bought
// the same full credit as $10M of real depth.
func TestLiquidityFactor_AnchorPoints(t *testing.T) {
	cases := []struct {
		usd       float64
		want, tol float64
		why       string
	}{
		{usd: 100, want: 0, tol: 0.01, why: "below floor"},
		{usd: 1_000, want: 0, tol: 0.01, why: "exactly floor"},
		{usd: 10_000, want: 1.0 / 3.0, tol: 1e-9, why: "min_usd_volume publish floor: ln(10)/ln(1000)"},
		{usd: 100_000, want: 2.0 / 3.0, tol: 1e-9, why: "the OLD ceiling is now two-thirds up the curve"},
		{usd: 123_678, want: 0.69743081788140493, tol: 1e-9, why: "BTC/USD p50 bucket — no longer saturating"},
		{usd: 1_000_000, want: 1.0, tol: 1e-12, why: "exactly ceiling"},
		{usd: 10_000_000, want: 1.0, tol: 1e-12, why: "above ceiling"},
	}
	for _, c := range cases {
		got := confidence.LiquidityFactor(c.usd)
		if !near(got, c.want, c.tol) {
			t.Errorf("LiquidityFactor(%v) = %v, want ~%v (%s)", c.usd, got, c.want, c.why)
		}
	}
}

func TestLiquidityFactor_LogShape(t *testing.T) {
	// The factor is a log-interpolation, so its 0.5 point is the
	// GEOMETRIC midpoint of [floor, ceiling] — sqrt(1e3 × 1e6) ≈
	// $31,622.78 since the 2026-07-25 ceiling change (it was $10K when
	// the ceiling was $100K).
	midpoint := math.Sqrt(1_000.0 * 1_000_000.0)
	if got := confidence.LiquidityFactor(midpoint); !near(got, 0.5, 1e-12) {
		t.Errorf("LiquidityFactor(%.2f) = %v, want 0.5 (geometric midpoint of the band)", midpoint, got)
	}
	// Monotone and strictly interior between the endpoints — the shape
	// claim, independent of where the ends sit.
	if a, b := confidence.LiquidityFactor(5_000), confidence.LiquidityFactor(500_000); !(a > 0 && a < b && b < 1) {
		t.Errorf("factor not strictly increasing inside the band: f(5K)=%v f(500K)=%v", a, b)
	}
}

// TestLiquidityFactor_GuardsBadInputs — NaN is a caller bug, not a
// sentinel, and must not poison the geometric mean. A MEASURED zero
// stays 0: "we looked and there is nothing here" is a real signal.
// (Negative inputs moved to TestLiquidityFactor_UnmeasuredIsNeutral
// when COR-14 made them the explicit "unmeasured" sentinel.)
func TestLiquidityFactor_GuardsBadInputs(t *testing.T) {
	for _, in := range []float64{math.NaN(), 0.0} {
		got := confidence.LiquidityFactor(in)
		if got != 0 {
			t.Errorf("LiquidityFactor(%v) = %v, want 0", in, got)
		}
	}
}

// TestLiquidityFactor_UnmeasuredIsNeutral (COR-14) — the negative
// sentinel means "we could not value this pair in USD at all", which
// is not a finding about the pair's liquidity and must NOT zero the
// factor (and with it, the whole geometric mean).
//
// Proven red against the pre-fix factor, which returned 0 for every
// negative input: each case reported 0 instead of 0.5.
func TestLiquidityFactor_UnmeasuredIsNeutral(t *testing.T) {
	for _, in := range []float64{confidence.LiquidityUnmeasured, -1.0, -0.5, math.Inf(-1)} {
		got := confidence.LiquidityFactor(in)
		if got != confidence.LiquidityUnmeasuredFactor {
			t.Errorf("LiquidityFactor(%v) = %v, want the neutral %v",
				in, got, confidence.LiquidityUnmeasuredFactor)
		}
	}
	// The neutral value must sit strictly between "measured thin" and
	// "measured deep" — a neutral that equalled either end would make
	// the sentinel indistinguishable from a real measurement.
	if confidence.LiquidityUnmeasuredFactor <= confidence.LiquidityFactor(liquidityFloorForTest) ||
		confidence.LiquidityUnmeasuredFactor >= confidence.LiquidityFactor(1_000_000) {
		t.Errorf("neutral %v is not strictly between the floor and ceiling factors",
			confidence.LiquidityUnmeasuredFactor)
	}
}

// liquidityFloorForTest mirrors the package's unexported
// liquidityFloorUSD; the factor at (and below) it is 0.
const liquidityFloorForTest = 1_000.0

// ─── CrossOracleFactor ────────────────────────────────────────────

func TestCrossOracleFactor_AnchorPoints(t *testing.T) {
	if got := confidence.CrossOracleFactor(0); !near(got, 1.0, 0.01) {
		t.Errorf("0%% divergence = %v, want ~1.0", got)
	}
	if got := confidence.CrossOracleFactor(5); !near(got, 0.5, 0.05) {
		t.Errorf("5%% divergence = %v, want ~0.5", got)
	}
	if got := confidence.CrossOracleFactor(50); got > 0.05 {
		t.Errorf("50%% divergence = %v, want near-0", got)
	}
}

func TestCrossOracleFactor_NoDataReturnsNeutral(t *testing.T) {
	// Negative input is the "no cross-oracle data" sentinel.
	got := confidence.CrossOracleFactor(-1)
	if got != 0.7 {
		t.Errorf("no-data sentinel = %v, want 0.7 (ADR-0019 worked example)", got)
	}
}

func TestCrossOracleFactor_GuardsNaN(t *testing.T) {
	if got := confidence.CrossOracleFactor(math.NaN()); got != 0 {
		t.Errorf("NaN = %v, want 0", got)
	}
}

// ─── TriangulationAgreementFactor ─────────────────────────────────

// TestTriangulationAgreementFactor_AnchorPoints pins the corroboration
// curve: full credit inside the 2% tolerance (which exists to absorb a
// chained-fiat leg snapped to a DAILY fx_quotes bucket, not to be
// lenient), halving every 4 percentage points past it, and near-zero
// for the divergence a single-venue manipulation on a thin pair
// produces.
func TestTriangulationAgreementFactor_AnchorPoints(t *testing.T) {
	cases := []struct {
		divergencePct float64
		want, tol     float64
		why           string
	}{
		{divergencePct: 0, want: 1.0, tol: 1e-12, why: "identical prices"},
		{divergencePct: 2.0, want: 1.0, tol: 1e-12, why: "exactly the tolerance — still full credit"},
		{divergencePct: 6.0, want: 0.5, tol: 1e-9, why: "tolerance + one half-life"},
		{divergencePct: 10.0, want: 0.25, tol: 1e-9, why: "tolerance + two half-lives"},
		{divergencePct: 25.0, want: 0.018581361171917517, tol: 1e-9, why: "manipulation-shaped gap"},
	}
	for _, c := range cases {
		got := confidence.TriangulationAgreementFactor(c.divergencePct)
		if !near(got, c.want, c.tol) {
			t.Errorf("TriangulationAgreementFactor(%v%%) = %v, want ~%v (%s)",
				c.divergencePct, got, c.want, c.why)
		}
	}
}

// TestTriangulationAgreementFactor_IsMonotoneDecreasing — the property
// that makes disagreement a signal rather than noise: more divergence
// can never score better.
func TestTriangulationAgreementFactor_IsMonotoneDecreasing(t *testing.T) {
	prev := confidence.TriangulationAgreementFactor(0)
	for d := 0.5; d <= 60; d += 0.5 {
		got := confidence.TriangulationAgreementFactor(d)
		if got > prev {
			t.Fatalf("factor rose at %v%%: %v > %v", d, got, prev)
		}
		prev = got
	}
	if prev > 0.01 {
		t.Errorf("factor at 60%% divergence = %v, want near-0", prev)
	}
}

func TestTriangulationAgreementFactor_NoCompositeReturnsNeutral(t *testing.T) {
	// Negative is the "no composite for this pair" sentinel — the same
	// 0.7 neutral CrossOracleFactor uses. Compute additionally zeroes
	// the factor's weight in this case; see
	// TestCompute_TriangulationUncheckedIsInert.
	if got := confidence.TriangulationAgreementFactor(-1); got != 0.7 {
		t.Errorf("no-composite sentinel = %v, want 0.7", got)
	}
}

func TestTriangulationAgreementFactor_GuardsNaN(t *testing.T) {
	if got := confidence.TriangulationAgreementFactor(math.NaN()); got != 0 {
		t.Errorf("NaN = %v, want 0", got)
	}
}

// ─── BaselineQualityFactor ────────────────────────────────────────

func TestBaselineQualityFactor(t *testing.T) {
	cases := []struct {
		days      float64
		want, tol float64
	}{
		{0, 0.5, 0.001},
		{15, 0.75, 0.001}, // halfway through ramp
		{30, 1.0, 0.001},
		{100, 1.0, 0.001}, // capped
	}
	for _, c := range cases {
		got := confidence.BaselineQualityFactor(c.days)
		if !near(got, c.want, c.tol) {
			t.Errorf("BaselineQualityFactor(%v days) = %v, want %v", c.days, got, c.want)
		}
	}
}

func TestBaselineQualityFactor_NegativeOrNaN_ReturnsBootstrap(t *testing.T) {
	// Negative or NaN treated as bootstrap, not failure — clock skew
	// shouldn't crater confidence.
	for _, in := range []float64{-1, math.NaN()} {
		got := confidence.BaselineQualityFactor(in)
		if got != 0.5 {
			t.Errorf("BaselineQualityFactor(%v) = %v, want 0.5", in, got)
		}
	}
}
