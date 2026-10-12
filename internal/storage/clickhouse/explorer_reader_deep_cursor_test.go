package clickhouse

import (
	"strings"
	"testing"
)

// A deep operations cursor must not scan O(table): ClickHouse cannot index a tuple comparison, so only
// `ledger_seq >= lower` prunes and a publicly mintable `?cursor=` reads up to the tip. These tests pin the
// rewritten SQL shape so the tuple-only form cannot return.

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
		// Parenthesisation is load-bearing: AND binds tighter than OR, so without it `AND ledger_seq >= ?` and the
		// LIMIT-BY tail attach to the equality arm alone and return every operation below the cursor.
		if !strings.Contains(q, recentOperationsCursorPredicate) ||
			!strings.HasPrefix(recentOperationsCursorPredicate, "(") ||
			!strings.HasSuffix(recentOperationsCursorPredicate, ")") {
			t.Errorf("cursor predicate is not wrapped in its own parentheses (bounded=%v):\n%s", bounded, q)
		}
	}
	// The tail-window lower bound must survive on the bounded arm — the rewrite is
	// what makes that bound bite, not a replacement for it.
	if !strings.Contains(recentOperationsQuery(true, true), "AND ledger_seq >= ?") {
		t.Error("the bounded cursor arm lost #444's tail-window lower bound")
	}
}

// The row budget backstops shapes the rewrite cannot bound (the unbounded near-tip fallback). It belongs
// on the CURSOR arms only: the unbounded first-page fallback announces a huge row count and takes no
// caller input.
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

// TestRecentOperationsCursor_RewriteIsExactlyEquivalent evaluates both predicate forms over an exhaustive
// ledger to pin (L,T,O) < (l,t,o) <=> L < l or (L = l and (T,O) < (t,o)); dropping the equality arm skips or
// repeats boundary rows.
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
