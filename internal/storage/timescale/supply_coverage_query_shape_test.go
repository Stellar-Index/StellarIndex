package timescale

import "testing"

// TestSupplyCoverageStatsQuery_NoRedundantTieBreak pins the `newest` CTE's ORDER BY to `time`
// alone. close_time is monotonic in ledger sequence, so a `ledger_sequence DESC` tie-break can
// never fire; it only adds an uncovered sort key and defeats the single ordered-append seek.
func TestSupplyCoverageStatsQuery_NoRedundantTieBreak(t *testing.T) {
	t.Parallel()

	if !sqlContainsFold(supplyCoverageStatsQuery, "ORDER BY time DESC") {
		t.Errorf("supplyCoverageStatsQuery must order the newest CTE by time DESC: %s", supplyCoverageStatsQuery)
	}
	if sqlContainsFold(supplyCoverageStatsQuery, "ledger_sequence DESC") {
		t.Errorf("supplyCoverageStatsQuery must not tie-break on ledger_sequence: %s", supplyCoverageStatsQuery)
	}
}
