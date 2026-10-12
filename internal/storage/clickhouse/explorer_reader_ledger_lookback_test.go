package clickhouse

import (
	"strings"
	"testing"
)

// TestLatestLedgerAtOrBeforeQuery_CarriesLowerBound pins the lower bound that keeps the lake-tip lookup
// bounded: `ledger_seq <= X` alone prunes no partition. The window is LatestLedgerLookbackLedgers, matching
// the callers' clamps.
func TestLatestLedgerAtOrBeforeQuery_CarriesLowerBound(t *testing.T) {
	q := latestLedgerAtOrBeforeQuery
	if !strings.Contains(q, "ledger_seq BETWEEN ? AND ?") {
		t.Errorf("lake-tip lookup has no lower bound on ledger_seq — an at-or-before predicate scans every partition below the cursor:\n%s", q)
	}
	if strings.Contains(q, "ledger_seq <= ?") {
		t.Errorf("lake-tip lookup is back to an unbounded at-or-before predicate:\n%s", q)
	}
	// FINAL is load-bearing here and not a scan-cost item: it collapses the
	// un-merged ReplacingMergeTree duplicates that concentrate at the tip
	// this window reads, and costs nothing measurable once bounded.
	if !strings.Contains(q, "FROM stellar.ledgers FINAL") {
		t.Errorf("lake-tip lookup lost FINAL — an un-merged duplicate part decides the snapshot's ObservedAt:\n%s", q)
	}
	if !strings.Contains(q, "ORDER BY ledger_seq DESC LIMIT 1") {
		t.Errorf("lake-tip lookup must still return the NEWEST landed row in the window:\n%s", q)
	}
}

// TestLatestLedgerLookbackFloor pins the scan bound to the clamp constant, so widening one without the other
// fails. It also pins the underflow: maxSeq below the lookback reads from 0 instead of wrapping uint32.
func TestLatestLedgerLookbackFloor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		maxSeq uint32
		want   uint32
	}{
		{"tip", 64_277_149, 64_277_149 - LatestLedgerLookbackLedgers},
		{"one past the bound", LatestLedgerLookbackLedgers + 1, 1},
		{"exactly the bound", LatestLedgerLookbackLedgers, 0},
		{"inside the bound", LatestLedgerLookbackLedgers - 1, 0},
		{"genesis", 1, 0},
		{"zero", 0, 0},
	} {
		if got := latestLedgerLookbackFloor(tc.maxSeq); got != tc.want {
			t.Errorf("%s: latestLedgerLookbackFloor(%d) = %d, want %d", tc.name, tc.maxSeq, got, tc.want)
		}
	}
	if LatestLedgerLookbackLedgers != 512 {
		t.Errorf("LatestLedgerLookbackLedgers = %d, want 512 — it is also both callers' stalled-lake refusal bound (~45 min of chain); changing it changes when a supply snapshot refuses to publish", LatestLedgerLookbackLedgers)
	}
}
