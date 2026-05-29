package timescale

import (
	"testing"
	"time"
)

func TestExpectedLedgersFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		genesis int64
		tip     int64
		want    int64
	}{
		{"happy path", 100, 1000, 901},
		{"tip == genesis", 500, 500, 1},
		{"zero genesis", 0, 1000, 0},
		{"zero tip", 100, 0, 0},
		{"tip < genesis (invalid)", 1000, 100, 0},
		{"negative genesis", -1, 100, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpectedLedgersFor(tc.genesis, tc.tip); got != tc.want {
				t.Errorf("ExpectedLedgersFor(%d, %d) = %d; want %d", tc.genesis, tc.tip, got, tc.want)
			}
		})
	}
}

func TestPercentagesFromCounts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		distinct    int64
		expected    int64
		maxGap      int64
		wantDensity float64
		wantGapFree float64
	}{
		{"clean dense source", 100, 100, 0, 1.0, 1.0},
		{"50% covered, no gap", 50, 100, 0, 0.5, 1.0},
		{"100% covered, gap somehow exists (impossible but defensive)", 100, 100, 50, 1.0, 0.5},
		{"sparse but gap-free", 1, 100, 0, 0.01, 1.0},
		{"sparse with gap", 1, 100, 50, 0.01, 0.5},
		{"zero expected", 0, 0, 0, 0, 0},
		{"density overflow capped at 1", 200, 100, 0, 1.0, 1.0},
		{"gap-free clamped at 0 when gap > expected", 50, 100, 200, 0.5, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			density, gapFree := percentagesFromCounts(tc.distinct, tc.expected, tc.maxGap)
			if density != tc.wantDensity {
				t.Errorf("density = %v; want %v", density, tc.wantDensity)
			}
			if gapFree != tc.wantGapFree {
				t.Errorf("gapFree = %v; want %v", gapFree, tc.wantGapFree)
			}
		})
	}
}

func TestSourceCoverageFromCounts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	cov := SourceCoverageFromCounts("blend-positions", "blend_positions", 950, 1000, 30, 3, now)
	if cov.Source != "blend-positions" {
		t.Errorf("Source = %q; want blend-positions", cov.Source)
	}
	if cov.Table != "blend_positions" {
		t.Errorf("Table = %q; want blend_positions", cov.Table)
	}
	if cov.DistinctLedgers != 950 || cov.ExpectedLedgers != 1000 {
		t.Errorf("count fields wrong: %+v", cov)
	}
	if cov.DensityPct != 0.95 {
		t.Errorf("DensityPct = %v; want 0.95", cov.DensityPct)
	}
	if cov.GapFreePct != 0.97 {
		t.Errorf("GapFreePct = %v; want 0.97", cov.GapFreePct)
	}
	if !cov.LastUpdated.Equal(now) {
		t.Errorf("LastUpdated = %v; want %v", cov.LastUpdated, now)
	}
}
