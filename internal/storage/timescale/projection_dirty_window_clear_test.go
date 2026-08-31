package timescale

import (
	"strings"
	"testing"
)

// TestClearProjectionDirtyWindowGuardsOnUpdatedAt pins the wave-D CV-6
// fix at the level a unit test can reach: the DELETE's own predicate.
//
// The clear's bounds (`from_ledger >= $2 AND to_ledger <= $3`) catch a
// WIDENED window — a concurrent replay that grew the range leaves a row
// outside the bounds, which survives, and the next run re-checks it.
//
// They do NOT catch a SUBSET RE-RECORD. A replay that re-records the
// same or a narrower range leaves both bounds satisfying the predicate,
// so the delete removes evidence of a rewind this run never verified,
// and the next completeness verdict carries a clean claim over it.
//
// `updated_at = $4` closes that: the clear succeeds only if the row is
// the one whose obligation this run actually discharged. Any re-record —
// wider, narrower, or identical — bumps updated_at, the delete matches
// nothing, and the window stays pending. Fail-closed, costing at most
// one extra reconcile.
//
// Asserted against the query text because the behaviour lives in SQL the
// database evaluates; the end-to-end path needs Postgres and is covered
// by the integration suite.
func TestClearProjectionDirtyWindowGuardsOnUpdatedAt(t *testing.T) {
	t.Parallel()
	q := clearProjectionDirtyWindowQuery

	if !strings.Contains(q, "updated_at = $4") {
		t.Error("the clear does not compare updated_at — a subset or identical " +
			"re-record between this run's read and its clear would be deleted, " +
			"erasing an unverified rewind and letting the next verdict carry a " +
			"clean claim over it")
	}
	// The bounds must stay: they are what makes a WIDENED window survive.
	// updated_at alone would delete a widened row whose bounds no longer
	// match this run's obligation.
	for _, want := range []string{"from_ledger >= $2", "to_ledger <= $3", "source = $1"} {
		if !strings.Contains(q, want) {
			t.Errorf("the clear lost its %q predicate — the bounds and the "+
				"updated_at guard cover different races and both are required", want)
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(q), "DELETE FROM projection_dirty_windows") {
		t.Errorf("unexpected statement shape: %q", strings.TrimSpace(q))
	}
}
