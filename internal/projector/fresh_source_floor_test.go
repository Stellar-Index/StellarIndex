package projector

import "testing"

// TestFreshSourceFloor pins RLT-154 / GH #623: a brand-new CH-mode source
// (no cursor row) must never hand ledger 0 to ContiguousWatermark — that
// underflows uint32 in watermark() and disables the stall-at-a-hole clamp
// forever for exactly this caller. The floor must land on a real ledger
// (>= 1) whether the lake is empty (lakeMin=0) or already populated
// (lakeMin=2, every net's actual first ledger).
func TestFreshSourceFloor(t *testing.T) {
	cases := []struct {
		name    string
		lakeMin uint32
		want    uint32
	}{
		{"empty lake floors at 1, never 0", 0, 1},
		{"populated lake floors at its real first ledger", 2, 2},
		{"lake starting above 2 is respected", 63_000_000, 63_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := freshSourceFloor(tc.lakeMin); got != tc.want {
				t.Fatalf("freshSourceFloor(%d) = %d, want %d", tc.lakeMin, got, tc.want)
			}
		})
	}
}
