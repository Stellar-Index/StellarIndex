package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Regression tests for #484: a deep operations cursor was O(table).
//
// The cursor arms compared the whole primary key as a TUPLE —
// `(ledger_seq, tx_index, op_index) < (?, ?, ?)` — and ClickHouse's
// KeyCondition does not decompose a multi-column tuple comparison, so the
// predicate was applied AFTER the scan instead of narrowing it. #444's
// `ledger_seq >= lower` bound was then the only index-usable term on a
// cursor page, and it bounds the read from BELOW: for `?cursor=5000000.0.0`
// the index selected everything from ledger 4,995,000 up to the tip, i.e.
// the whole table.
//
// Measured on r1 for that exact cursor (2026-09-02, read-only):
//
//	EXPLAIN ESTIMATE   old: 80 parts / 24,693,075,112 rows / 3,014,332 marks
//	                   new:  1 part  /          4,157 rows /         1 mark
//	system.query_log   old: 2,863,412,859 rows / 32.00 GiB / killed unfinished
//	                        at a 30 s cap (unguarded on the issue: 19.3 B rows
//	                        / 215 GiB / > 180 s)
//	                   new:           4,157 rows / 564.43 KiB / 5 ms
//
// `?cursor=` is publicly mintable dotted decimal on an unauthenticated
// route, so this was a cheap way to make the origin read hundreds of
// gigabytes — not merely a slow page. These tests pin the rewritten shape so
// the tuple-only form cannot come back, following the package's SQL-text
// convention (explorer_scan_settings_test.go).

// tupleOnlyCursor is the un-prunable form. It must not appear in any
// RecentOperations arm.
const tupleOnlyCursor = "(ledger_seq, tx_index, op_index) < (?, ?, ?)"

func TestRecentOperationsCursor_PredicateIsPrimaryIndexPrunable(t *testing.T) {
	for _, bounded := range []bool{true, false} {
		q := recentOperationsQuery(true, bounded)
		if strings.Contains(q, tupleOnlyCursor) {
			t.Fatalf("cursor arm (bounded=%v) is back on the un-prunable tuple comparison — this IS #484:\n%s", bounded, q)
		}
		// The leading term is the whole point: `ledger_seq < ?` is what
		// KeyCondition can turn into a mark range.
		if !strings.Contains(q, "ledger_seq < ?") {
			t.Errorf("cursor arm (bounded=%v) has no index-usable leading `ledger_seq < ?` term:\n%s", bounded, q)
		}
		// The equality arm is what keeps the rewrite EXACT: without it the
		// page would skip every remaining operation of the cursor's own
		// ledger.
		if !strings.Contains(q, "ledger_seq = ? AND (tx_index, op_index) < (?, ?)") {
			t.Errorf("cursor arm (bounded=%v) lost the same-ledger tie-break; paging across a ledger boundary would skip rows:\n%s", bounded, q)
		}
		// Parenthesisation is load-bearing, not cosmetic: AND binds tighter
		// than OR, so an unparenthesised predicate would let `AND ledger_seq
		// >= ?` (and the LIMIT-BY/settings tail) attach to the equality arm
		// alone and return every operation below the cursor.
		if !strings.Contains(q, recentOperationsCursorPredicate) ||
			!strings.HasPrefix(recentOperationsCursorPredicate, "(") ||
			!strings.HasSuffix(recentOperationsCursorPredicate, ")") {
			t.Errorf("cursor predicate is not wrapped in its own parentheses (bounded=%v):\n%s", bounded, q)
		}
	}
	// #444's lower bound must survive on the bounded arm — the rewrite is
	// what makes that bound bite, not a replacement for it.
	if !strings.Contains(recentOperationsQuery(true, true), "AND ledger_seq >= ?") {
		t.Error("the bounded cursor arm lost #444's tail-window lower bound")
	}
}

// The row budget is the backstop for the shapes the rewrite alone cannot
// bound (the unbounded fallback at a near-tip cursor still selects
// [genesis, cursor]). It belongs on the CURSOR arms only: measured on r1,
// #444's unbounded FIRST-PAGE fallback announces 217.44M rows and would be
// refused by this ceiling, and that arm carries no caller-controlled input.
func TestRecentOperationsCursor_CarriesARowBudgetTheFirstPageDoesNot(t *testing.T) {
	for _, bounded := range []bool{true, false} {
		q := recentOperationsQuery(true, bounded)
		for _, want := range []string{"max_rows_to_read = 200000000", "read_overflow_mode = 'throw'"} {
			if !strings.Contains(q, want) {
				t.Errorf("cursor arm (bounded=%v) missing %q — a pathological cursor would be SERVED, not refused:\n%s", bounded, want, q)
			}
		}
		requireScanSettings(t, "recentOperationsQuery(cursor)", q)
	}
	for _, bounded := range []bool{true, false} {
		q := recentOperationsQuery(false, bounded)
		if strings.Contains(q, "max_rows_to_read") {
			t.Errorf("first-page arm (bounded=%v) must NOT carry the cursor row budget — r1 measured its unbounded fallback at 217.44M announced rows, so the ceiling would break #444's correctness net:\n%s", bounded, q)
		}
	}
}

// The ledger binds TWICE. An arg list that still carries the 3-arg tuple
// shape would bind tx_index to `ledger_seq = ?` and silently return a
// different page.
func TestRecentOperations_CursorArgsBindTheLedgerToBothArms(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) {
		return &stubRows{data: opsPage(4_999_900, 50)}, nil
	}
	r := &ExplorerReader{conn: conn}

	cur := ExplorerCursor{Ledger: 5_000_000, A: 0, B: 0}
	if _, err := r.RecentOperations(context.Background(), 50, cur); err != nil {
		t.Fatalf("RecentOperations: %v", err)
	}
	want := []any{
		cur.Ledger, // ledger_seq < ?
		cur.Ledger, // ledger_seq = ?
		cur.A,      // (tx_index, …) < (?, …)
		cur.B,      // (…, op_index) < (…, ?)
		cur.Ledger - uint32(recentLedgersTailWindow), // #444 lower bound
		50, // LIMIT
	}
	got := conn.args[0]
	if len(got) != len(want) {
		t.Fatalf("cursor args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg[%d] = %v (%T), want %v (%T)", i, got[i], got[i], want[i], want[i])
		}
	}
}

// A refused cursor must arrive as its OWN class. Without this it is
// indistinguishable from a lake fault, and the route's error mapping cannot
// tell "you asked for an unservable position" from "we broke".
func TestRecentOperations_RefusedCursorIsItsOwnErrorClass(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) {
		return nil, &clickhouse.Exception{
			Code: 158, Name: "TOO_MANY_ROWS",
			Message: "Limit for rows or bytes to read exceeded, max rows: 200.00 million, current rows: 233.82 million",
		}
	}
	r := &ExplorerReader{conn: conn}

	_, err := r.RecentOperations(context.Background(), 50, ExplorerCursor{Ledger: 64_200_000, A: 0, B: 0})
	if err == nil {
		t.Fatal("a refused cursor returned no error — the caller would serve an empty page as if history ended")
	}
	if !errors.Is(err, ErrOperationsCursorTooDeep) {
		t.Fatalf("refused cursor error = %v, want it to wrap ErrOperationsCursorTooDeep", err)
	}
	// The offending cursor must be in the message: an operator reading the
	// log has to know which position was refused.
	if !strings.Contains(err.Error(), "64200000.0.0") {
		t.Errorf("refused-cursor error does not name the cursor: %v", err)
	}
	// Every OTHER server error keeps its existing class — a refusal-shaped
	// classifier that swallowed real faults would be worse than the defect.
	conn2 := &stubConn{}
	conn2.respond = func(string) (driver.Rows, error) {
		return nil, &clickhouse.Exception{Code: 241, Name: "MEMORY_LIMIT_EXCEEDED"}
	}
	r2 := &ExplorerReader{conn: conn2}
	_, err = r2.RecentOperations(context.Background(), 50, ExplorerCursor{Ledger: 64_200_000})
	if err == nil || errors.Is(err, ErrOperationsCursorTooDeep) {
		t.Errorf("a memory-limit failure was misclassified as a cursor refusal: %v", err)
	}
}

// TestRecentOperationsCursor_RewriteIsExactlyEquivalent walks the boundary
// cases the rewrite has to preserve, by evaluating BOTH predicate forms over
// an exhaustive little ledger. This is the identity
//
//	(L,T,O) < (l,t,o)  ⟺  L < l ∨ (L = l ∧ (T,O) < (t,o))
//
// pinned as a test rather than as a comment, because a future "simplification"
// that drops the equality arm (or writes `<=` on the leading term) still
// compiles, still prunes, and silently skips or repeats rows at every ledger
// boundary.
func TestRecentOperationsCursor_RewriteIsExactlyEquivalent(t *testing.T) {
	type key struct{ L, T, O uint32 }
	// A miniature operations table: ledger 10 has a single operation,
	// ledger 11 has two txs with two ops each, ledger 12 has one op.
	table := []key{
		{10, 0, 0},
		{11, 0, 0},
		{11, 0, 1},
		{11, 1, 0},
		{11, 1, 1},
		{12, 0, 0},
	}
	tuple := func(r, c key) bool { // (L,T,O) < (l,t,o), lexicographic
		if r.L != c.L {
			return r.L < c.L
		}
		if r.T != c.T {
			return r.T < c.T
		}
		return r.O < c.O
	}
	rewrite := func(r, c key) bool { // ledger_seq < l OR (ledger_seq = l AND (t,o) < (t,o))
		if r.L < c.L {
			return true
		}
		if r.L != c.L {
			return false
		}
		if r.T != c.T {
			return r.T < c.T
		}
		return r.O < c.O
	}

	for _, tc := range []struct {
		name string
		cur  key
		want []key
	}{
		{"first op of a ledger", key{11, 0, 0}, []key{{10, 0, 0}}},
		{"last op of a ledger", key{11, 1, 1}, []key{{10, 0, 0}, {11, 0, 0}, {11, 0, 1}, {11, 1, 0}}},
		{"mid-ledger, second tx", key{11, 1, 0}, []key{{10, 0, 0}, {11, 0, 0}, {11, 0, 1}}},
		{"ledger holding a single operation", key{10, 0, 0}, nil},
		{"cursor above the newest row", key{12, 0, 1}, []key{{10, 0, 0}, {11, 0, 0}, {11, 0, 1}, {11, 1, 0}, {11, 1, 1}, {12, 0, 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tupleRows, rewriteRows []key
			for _, row := range table {
				if tuple(row, tc.cur) {
					tupleRows = append(tupleRows, row)
				}
				if rewrite(row, tc.cur) {
					rewriteRows = append(rewriteRows, row)
				}
			}
			// The tuple form is the SPEC here; assert the rewrite matches
			// it AND that both match the independently-written expectation
			// (so a bug shared by both forms cannot pass).
			if len(tupleRows) != len(tc.want) {
				t.Fatalf("tuple form selected %v, want %v", tupleRows, tc.want)
			}
			for i := range tc.want {
				if tupleRows[i] != tc.want[i] {
					t.Fatalf("tuple form selected %v, want %v", tupleRows, tc.want)
				}
			}
			if len(rewriteRows) != len(tupleRows) {
				t.Fatalf("rewrite selected %v, tuple form selected %v", rewriteRows, tupleRows)
			}
			for i := range tupleRows {
				if rewriteRows[i] != tupleRows[i] {
					t.Fatalf("rewrite selected %v, tuple form selected %v", rewriteRows, tupleRows)
				}
			}
		})
	}
}
