package clickhouse

import (
	"strings"
	"testing"
)

// TestLatestLedgerAtOrBeforeQuery_CarriesLowerBound pins the predicate that
// makes the supply snapshot's lake-tip lookup a bounded read.
//
// stellar.ledgers is PARTITION BY intDiv(ledger_seq, 1000000) ORDER BY ledger_seq, so
// `ledger_seq <= X` prunes NO partition below X and the descending LIMIT 1 does not rescue it
// under FINAL. The aggregator issues this once per watched asset per tick, so the unbounded
// shape reads the whole table repeatedly from a client that is not on the serving profile.
//
// The window is [maxSeq-LatestLedgerLookbackLedgers, maxSeq] and nothing wider, because a row
// older than that is refused by both callers
// (cmd/stellarindex-aggregator/main.go::maxSupplyLakeClampLedgers,
// internal/ops/supply/supply.go::maxAutoSnapshotClampLedgers, which ARE this constant).
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

// TestLatestLedgerLookbackFloor pins the bound to the clamp constant itself, so widening the scan
// without widening what callers accept (or the reverse) fails here. It also pins the underflow:
// maxSeq below the lookback reads from 0 rather than wrapping uint32, which would return no row
// on a young network and fail every snapshot closed.
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
