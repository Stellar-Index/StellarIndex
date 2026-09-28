package timescale

import "testing"

// TestSupplyCoverageStatsQuery_NoRedundantTieBreak pins the `newest` CTE's
// ORDER BY to `time` alone. close_time is monotonic in ledger sequence (no
// two ledgers share a close time), so a `ledger_sequence DESC` tie-break can
// never fire — it only forces the planner to carry an extra, uncovered sort
// key, defeating the single ordered-append seek the surrounding comment
// promises. A reintroduced tie-break must fail this test, not just look
// harmless.
func TestSupplyCoverageStatsQuery_NoRedundantTieBreak(t *testing.T) {
	t.Parallel()

	if !sqlContainsFold(supplyCoverageStatsQuery, "ORDER BY time DESC") {
		t.Errorf("supplyCoverageStatsQuery must order the newest CTE by time DESC: %s", supplyCoverageStatsQuery)
	}
	if sqlContainsFold(supplyCoverageStatsQuery, "ledger_sequence DESC") {
		t.Errorf("supplyCoverageStatsQuery must not tie-break on ledger_sequence: %s", supplyCoverageStatsQuery)
	}
}
