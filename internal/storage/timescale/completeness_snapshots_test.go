package timescale

import "testing"

// The CS-083 guard's problem arm: a found failure admits a lower-tip
// write whether or not it names a ledger; a verdict that is false only
// because a claim was not evaluated does not.
func TestCompletenessSnapshotFoundProblem(t *testing.T) {
	cases := []struct {
		name string
		snap CompletenessSnapshot
		want bool
	}{
		{"clean", CompletenessSnapshot{Complete: true, ProjectionOK: true}, false},
		{"located problem", CompletenessSnapshot{FirstProblem: 63_100_000}, true},
		{"CH reconcile mismatch, no ledger", CompletenessSnapshot{LakeComplete: true, FoundProblem: true}, true},
		{"projection not evaluated", CompletenessSnapshot{LakeComplete: true}, false},
		{"degenerate tip below genesis", CompletenessSnapshot{Genesis: 100, Tip: 50, Watermark: 99}, false},
	}
	for _, tc := range cases {
		if got := tc.snap.foundProblem(); got != tc.want {
			t.Errorf("%s: foundProblem() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
