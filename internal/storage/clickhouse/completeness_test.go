package clickhouse

import "testing"

func TestWatermark(t *testing.T) {
	tests := []struct {
		name             string
		from, chMax, gap uint32
		want             uint32
	}{
		{"ch not yet reached from", 100, 90, 0, 99},   // chMax<from → from-1 (idle)
		{"ch exactly at from-1", 100, 99, 0, 99},      // same boundary
		{"complete to tip, no gap", 100, 200, 0, 200}, // firstGap==0 → chMax
		{"gap above from", 100, 200, 150, 149},        // hole at 150 → 149
		{"gap right at from", 100, 200, 100, 99},      // hole at from itself → from-1 (idle)
		{"gap one past from", 100, 200, 101, 100},     // first ledger present, hole next
		{"single complete ledger at from", 100, 100, 0, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := watermark(tt.from, tt.chMax, tt.gap); got != tt.want {
				t.Errorf("watermark(from=%d, chMax=%d, gap=%d) = %d, want %d",
					tt.from, tt.chMax, tt.gap, got, tt.want)
			}
		})
	}
}

// TestSubstrateHeadProblem pins the F1 consumer fail-open fix: the coverage
// verdict must return a problem ledger that keeps the per-source consumer's
// `problem < genesis ⟹ source-OK` test correct. Empty ⟹ tip (so every source
// fails), missing-head ⟹ haveMin-1 (so only sources whose data starts in the
// absent head fail), covered ⟹ no problem.
func TestSubstrateHeadProblem(t *testing.T) {
	const from, to = uint32(2), uint32(63_000_000)
	cases := []struct {
		name        string
		present     bool
		haveMin     uint32
		wantProblem uint32
		wantHas     bool
	}{
		{"empty range returns tip so every source fails", false, 0, to, true},
		{"missing head returns haveMin-1", true, 50_000_000, 49_999_999, true},
		{"head present at from is covered", true, 2, 0, false},
		{"head present above from (from=2, haveMin=2) covered", true, from, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, has, _ := substrateHeadProblem(from, to, tc.present, tc.haveMin)
			if has != tc.wantHas || p != tc.wantProblem {
				t.Fatalf("substrateHeadProblem(present=%v, haveMin=%d) = (%d,%v), want (%d,%v)",
					tc.present, tc.haveMin, p, has, tc.wantProblem, tc.wantHas)
			}
		})
	}
	// The regression itself: an EMPTY lake must NOT green a high-genesis source.
	// Pre-fix this returned `from` (2); `2 < 50_746_266` was true = greened.
	// The problem ledger must be ≥ any source genesis so `problem < genesis` is
	// false for all (soroswap genesis = 50_746_266).
	if p, has, _ := substrateHeadProblem(from, to, false, 0); !has || p < 50_746_266 {
		t.Fatalf("empty-lake problem ledger = %d (has=%v); must be ≥ any source genesis so none green (soroswap=50746266)", p, has)
	}
}
