package chops

import "testing"

func TestTxIndexBackfillCoversHistory(t *testing.T) {
	cases := []struct {
		name string
		plan txIndexBackfillPlan
		want bool
	}{
		{"genesis to tip", txIndexBackfillPlan{from: 2, to: 0}, true},
		{"resume run", txIndexBackfillPlan{from: 5_000_000, to: 0}, false},
		{"explicit upper bound", txIndexBackfillPlan{from: 2, to: 1_000}, false},
	}
	for _, tc := range cases {
		if got := tc.plan.coversHistory(); got != tc.want {
			t.Errorf("%s: coversHistory = %v, want %v", tc.name, got, tc.want)
		}
	}
}
