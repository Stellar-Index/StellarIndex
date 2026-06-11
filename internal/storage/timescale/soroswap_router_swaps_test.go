package timescale

import (
	"testing"
	"time"
)

// TestPgTimestamptzRepresentable guards the clamp that keeps a garbage router
// `deadline` from rejecting an otherwise-valid swap. A u64 deadline sentinel
// (~3e18 s) renders a year-99-billion time, and a deadline above max-int64
// overflows int64(deadline) to a negative (BC) time; both are outside
// Postgres's timestamptz range and error the whole INSERT (SQLSTATE 22008).
// Before this clamp, ~24% of historical soroswap-router swaps were silently
// dropped by both the live indexer and every backfill.
func TestPgTimestamptzRepresentable(t *testing.T) {
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"recent deadline", time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC), true},
		{"epoch", time.Unix(0, 0).UTC(), true},
		{"far-but-valid future", time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), true},
		// u64 sentinel deadline ≈ 3.1e18 seconds → year ~99 billion.
		{"u64 sentinel deadline", time.Unix(3_100_000_000_000_000_000, 0).UTC(), false},
		// deadline > max-int64 overflows int64() to a large negative → BC year.
		{"int64-overflow BC", time.Unix(-3_100_000_000_000_000_000, 0).UTC(), false},
		{"just past pg max", pgTimestamptzMax.Add(time.Hour), false},
		{"just before pg min", pgTimestamptzMin.Add(-time.Hour), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pgTimestamptzRepresentable(c.t); got != c.want {
				t.Errorf("pgTimestamptzRepresentable(%v) = %v, want %v", c.t, got, c.want)
			}
		})
	}
}
