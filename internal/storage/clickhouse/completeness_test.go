package clickhouse

import "testing"

// TestSubstrateQueryLo pins FINDING 2: the per-window substrate query must start
// one ledger BELOW the window so the seam hash-link is checked — including at
// the LOWER boundary `from`, the carried/fresh -from junction. The old guard
// only decremented when the window started ABOVE `from` (inter-window seams), so
// the FIRST window's query lo stayed at `from`; chainQ's `WHERE ledger_seq > qlo`
// then never evaluated prev_hash(from) == ledger_hash(from-1), and a hash-chain
// break exactly at the scanFrom seam was invisible even to the window abutting
// it. The fix decrements to from-1, guarded at the chain genesis (ledger 1 has
// no predecessor to link) so it never underflows.
func TestSubstrateQueryLo(t *testing.T) {
	const window = uint64(substrateWindow)
	cases := []struct {
		name      string
		wlo, from uint64
		want      uint64
	}{
		// The FINDING: an incremental -from run's first window. Pre-fix this
		// returned `from` (63_300_000) and the seam link at `from` went
		// unchecked; the fix returns from-1 so the link is evaluated.
		{"lower-boundary seam is checked (the finding)", 63_300_000, 63_300_000, 63_299_999},
		// A full scan from the default floor still checks its seam link at
		// ledger 2 (prev_hash(2) == ledger_hash(1)) — previously missed.
		{"full-scan floor (from=2) checks the link at ledger 2", 2, 2, 1},
		// The chain genesis has no predecessor: no underflow, no phantom link.
		{"chain genesis (from=1) does not underflow", 1, 1, 1},
		// Inter-window seams are unchanged by the fix.
		{"inter-window seam still decrements", 2 + window, 2, 2 + window - 1},
		{"interior inter-window seam still decrements", 63_300_000 + window, 63_300_000, 63_300_000 + window - 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := substrateQueryLo(tc.wlo, tc.from); got != tc.want {
				t.Fatalf("substrateQueryLo(wlo=%d, from=%d) = %d, want %d — a seam hash-link would go unchecked",
					tc.wlo, tc.from, got, tc.want)
			}
		})
	}

	// The load-bearing property, stated directly: for the FIRST window
	// (wlo == from) at any real scan floor (from > chain genesis), the query
	// must reach one ledger below `from` so the boundary link is in range.
	for _, from := range []uint64{2, 51_000_000, 63_300_000} {
		if got := substrateQueryLo(from, from); got != from-1 {
			t.Fatalf("first-window lo for from=%d = %d, want from-1 %d (the boundary seam must be covered)", from, got, from-1)
		}
	}
}

// TestWatermark pins the pure interpretation of the ContiguousWatermark query,
// including the lower-boundary blind spot (a hole AT `from`). CRITICAL: the
// minPresent column is what the SQL actually produces — min(ledger_seq >= from) —
// so the inputs below are the ones the query can really return. The old table had
// a "gap right at from" row that fed gap=100 (firstGap==from); that is an input
// the SQL CANNOT produce for a missing `from` (a missing `from` leaves the set
// {from+1,…} internally contiguous → firstGap==0, minPresent==from+1), so it
// masked the silent-data-loss bug. Those masking synthetic-gap cases are replaced
// with the real hole-at-from shape below.
func TestWatermark(t *testing.T) {
	tests := []struct {
		name                         string
		from, chMax, gap, minPresent uint32
		want                         uint32
	}{
		// CH has not reached `from`: no ledger >= from exists, so minPresent==0.
		{"ch not yet reached from", 100, 90, 0, 0, 99}, // chMax<from → from-1 (idle)
		{"ch exactly at from-1", 100, 99, 0, 0, 99},    // same boundary
		// `from` present + contiguous to the tip: minPresent==from, firstGap==0.
		{"complete to tip, no gap", 100, 200, 0, 100, 200}, // firstGap==0 → chMax
		// `from` present, interior hole at 150: minPresent==from, firstGap==150.
		{"interior gap above from", 100, 200, 150, 100, 149}, // hole at 150 → 149
		// THE FIX — hole exactly at `from` (100 absent, 101..200 present). The SQL
		// yields firstGap==0 (the present set {101,…,200} is internally contiguous)
		// and minPresent==101 (the smallest ledger >= from). Pre-fix, the two-arg
		// watermark ignored minPresent and returned chMax (200), so the projector
		// scanned past the missing ledger 100 and dropped its sep41 rows. Now the
		// minPresent>from guard stalls at from-1 (99).
		{"HOLE AT from itself → stall at from-1", 100, 200, 0, 101, 99},
		// Hole at `from` AND an interior gap co-exist above it (100 absent;
		// 101..149 present; 150 absent; 151..200 present). firstGap==150,
		// minPresent==101. The boundary hole must win — from-1 (99) is the tighter
		// bound — so the projector stalls at the earliest defect, not at 149.
		{"hole at from wins over a later interior gap", 100, 200, 150, 101, 99},
		// `from` present, first ledger present then hole at 101: minPresent==from.
		{"gap one past from", 100, 200, 101, 100, 100}, // first ledger present, hole next
		{"single complete ledger at from", 100, 100, 0, 100, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := watermark(tt.from, tt.chMax, tt.gap, tt.minPresent); got != tt.want {
				t.Errorf("watermark(from=%d, chMax=%d, gap=%d, minPresent=%d) = %d, want %d",
					tt.from, tt.chMax, tt.gap, tt.minPresent, got, tt.want)
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
