package aggregate

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestFilterFreshAggregatorRows_LeavesInputUntouched pins that the freshness filter does not overwrite the caller's backing array.
func TestFilterFreshAggregatorRows_LeavesInputUntouched(t *testing.T) {
	now := time.Now()
	rows := []canonical.OracleUpdate{
		{Source: "stale", Timestamp: now.Add(-time.Hour)},
		{Source: "fresh", Timestamp: now},
	}
	got := filterFreshAggregatorRows(rows, time.Minute)
	if len(got) != 1 || got[0].Source != "fresh" {
		t.Fatalf("filtered = %+v, want only the fresh row", got)
	}
	if rows[0].Source != "stale" || rows[1].Source != "fresh" {
		t.Errorf("input mutated to [%s %s], want [stale fresh]", rows[0].Source, rows[1].Source)
	}
}

func TestFilterFreshAggregatorRows_ZeroMaxAgeKeepsEverything(t *testing.T) {
	old := mkAggRow("a", 1, 2)
	old.Timestamp = time.Unix(0, 0)
	if got := filterFreshAggregatorRows([]canonical.OracleUpdate{old}, 0); len(got) != 1 {
		t.Fatalf("maxAge 0 must not filter; kept %d/1", len(got))
	}
	if got := filterFreshAggregatorRows([]canonical.OracleUpdate{old}, time.Hour); len(got) != 0 {
		t.Fatalf("a 1970 row must be stale at maxAge 1h; kept %d/1", len(got))
	}
}
