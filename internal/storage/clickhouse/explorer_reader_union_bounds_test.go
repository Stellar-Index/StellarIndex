package clickhouse

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Regression tests for audit C-F1a: the two UNION arms in
// AccountOperations / AccountTransactions selected full rows with NO per-arm
// ORDER BY / LIMIT — only the outer query was bounded. A high-activity account
// therefore materialised EVERY row it ever touched (for operations, including
// the KB-scale body_xdr blob) before the outer LIMIT 50 discarded almost all of
// it; live-measured at 5–6 s on an otherwise idle box.
//
// These use the stubConn/stubRows harness from tx_hash_index_test.go. stubConn
// does not execute SQL, so the first two tests are query-SHAPE assertions
// (proof that the emitted SQL bounds each arm, and binds the arms to the SAME
// page size as the outer query). TestUnionArmTopN_MatchesUnboundedMerge then
// proves the correctness half — that bounding the arms cannot lose a row —
// against the per-arm limits the reader actually emitted.

// armBodies splits the emitted UNION query into its two arm bodies (the text
// between the parens either side of UNION ALL). Fails the test if the query
// isn't the expected two-arm shape.
func armBodies(t *testing.T, q string) (string, string) {
	t.Helper()
	parts := strings.Split(q, "UNION ALL")
	if len(parts) != 2 {
		t.Fatalf("query is not a two-arm UNION: %s", q)
	}
	return parts[0], parts[1]
}

// countPerArmLimitArgs returns how many of the bound args equal the page size —
// one per bounded arm plus one for the outer LIMIT.
func countPerArmLimitArgs(args []any, limit int) int {
	n := 0
	for _, a := range args {
		if v, ok := a.(int); ok && v == limit {
			n++
		}
	}
	return n
}

func TestAccountOperations_BoundsEachUnionArm(t *testing.T) {
	const limit = 37
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) {
		return &stubRows{data: [][]any{opRowFor(100, 0, 0)}}, nil
	}
	r := &ExplorerReader{conn: conn}

	if _, err := r.AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	q := conn.queries[0]
	arm1, arm2 := armBodies(t, q)

	// Both arms must ORDER BY the full sort key and take their own LIMIT.
	// Without this the arm reads every matching op — body_xdr included —
	// before the outer LIMIT throws it away (C-F1a).
	for i, arm := range []string{arm1, arm2} {
		if !strings.Contains(arm, "ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC") {
			t.Errorf("arm %d has no per-arm ORDER BY — it cannot read in reverse primary-key order and stop early: %s", i+1, arm)
		}
		if !strings.Contains(arm, "LIMIT ?") {
			t.Errorf("arm %d has no per-arm LIMIT — the whole matching history materialises before the outer LIMIT: %s", i+1, arm)
		}
		// The DAT-10 dedup must still run BEFORE the arm's LIMIT, so the
		// arm's N slots hold N DISTINCT primary keys.
		if !strings.Contains(arm, "LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?") {
			t.Errorf("arm %d must dedup (LIMIT 1 BY) before taking its LIMIT, else an un-merged duplicate part eats a slot: %s", i+1, arm)
		}
	}
	// Three bound page sizes: one per arm + the outer merge LIMIT. A per-arm
	// limit SMALLER than the outer one would silently drop rows at the seam.
	if n := countPerArmLimitArgs(conn.args[0], limit); n != 3 {
		t.Errorf("bound page-size args = %d, want 3 (arm1, arm2, outer) — each arm must be bounded by the SAME page size as the outer merge; args: %v", n, conn.args[0])
	}
}

func TestAccountTransactions_BoundsEachUnionArm(t *testing.T) {
	const limit = 37
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) {
		return &stubRows{data: [][]any{txRowFor(100, testTxHash)}}, nil
	}
	r := &ExplorerReader{conn: conn}

	if _, err := r.AccountTransactions(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountTransactions: %v", err)
	}
	q := conn.queries[0]
	arm1, arm2 := armBodies(t, q)

	for i, arm := range []string{arm1, arm2} {
		if !strings.Contains(arm, "ORDER BY ledger_seq DESC, tx_index DESC") {
			t.Errorf("arm %d has no per-arm ORDER BY: %s", i+1, arm)
		}
		if !strings.Contains(arm, "LIMIT 1 BY ledger_seq, tx_index LIMIT ?") {
			t.Errorf("arm %d must dedup on the primary key and then take its own LIMIT — otherwise an un-merged duplicate part consumes a slot and the page comes back short: %s", i+1, arm)
		}
	}
	if n := countPerArmLimitArgs(conn.args[0], limit); n != 3 {
		t.Errorf("bound page-size args = %d, want 3 (arm1, arm2, outer); args: %v", n, conn.args[0])
	}
	// The cross-arm dedup must survive: a tx can be BOTH sourced by the
	// account and carry it as a non-source participant.
	if !strings.HasPrefix(strings.TrimSpace(q), "SELECT DISTINCT") {
		t.Errorf("outer SELECT lost its DISTINCT — cross-arm duplicates would be served twice: %s", q)
	}
}

// TestAccountOperations_PerArmLimitPreservesCursorArgOrder guards the bind
// order: the arms' new LIMIT placeholders sit between the existing
// account/cursor placeholders, so a mis-ordered args slice would bind a ledger
// number as a page size (and vice versa) the moment a cursor page is served.
func TestAccountOperations_PerArmLimitPreservesCursorArgOrder(t *testing.T) {
	const limit = 9
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) { return &stubRows{}, nil }
	r := &ExplorerReader{conn: conn}

	cur := ExplorerCursor{Ledger: 63_000_000, A: 4, B: 2}
	if _, err := r.AccountOperations(context.Background(), "GTEST", limit, cur); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	want := []any{
		"GTEST", cur.Ledger, cur.A, cur.B, limit, // arm 1: account, cursor, page size
		"GTEST", cur.Ledger, cur.A, cur.B, limit, // arm 2: same
		limit, // outer merge
	}
	got := conn.args[0]
	if len(got) != len(want) {
		t.Fatalf("bound %d args, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d = %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}
}

// opKey is a row's sort key in the merged listing.
type opKey struct{ ledger, txIndex, opIndex uint32 }

// mergeTopN is the outer query's semantics: concatenate the arms, dedup by
// key, sort newest-first, take n.
func mergeTopN(arms [][]opKey, n int) []opKey {
	seen := map[opKey]bool{}
	var all []opKey
	for _, arm := range arms {
		for _, k := range arm {
			if seen[k] {
				continue
			}
			seen[k] = true
			all = append(all, k)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].ledger != all[j].ledger {
			return all[i].ledger > all[j].ledger
		}
		if all[i].txIndex != all[j].txIndex {
			return all[i].txIndex > all[j].txIndex
		}
		return all[i].opIndex > all[j].opIndex
	})
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// armTopN is one arm's new per-arm `ORDER BY … LIMIT n`.
func armTopN(arm []opKey, n int) []opKey { return mergeTopN([][]opKey{arm}, n) }

// TestUnionArmTopN_MatchesUnboundedMerge is the correctness half of C-F1a: the
// arms are now cut to their own top-N BEFORE the merge, so the fix is only
// safe if the union of two individually-top-N arms always contains the union's
// top N. It does — a row in the true top N has at most N-1 rows ahead of it
// across both arms, hence at most N-1 ahead of it within its own arm — and
// this pins that invariant against the page size the reader actually binds
// (read off the emitted query, so shrinking the per-arm limit breaks it).
//
// Fixtures cover the seam cases: arms fully interleaved, one arm strictly
// newer than the other (the case where a naive "half the limit per arm" split
// loses rows), cross-arm duplicates, and arms shorter than the page.
func TestUnionArmTopN_MatchesUnboundedMerge(t *testing.T) {
	const limit = 4

	// Read the per-arm page size out of the query the reader emits, so this
	// property is anchored to the implementation rather than to a constant.
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) { return &stubRows{}, nil }
	r := &ExplorerReader{conn: conn}
	if _, err := r.AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	args := conn.args[0]
	armLimit, ok := args[1].(int) // arm 1: [account, LIMIT]
	if !ok {
		t.Fatalf("arm-1 limit arg is %T, want int (args: %v)", args[1], args)
	}

	k := func(l, t, o uint32) opKey { return opKey{l, t, o} }
	cases := []struct {
		name       string
		arm1, arm2 []opKey
	}{
		{
			name: "interleaved",
			arm1: []opKey{k(100, 0, 0), k(98, 0, 0), k(96, 0, 0), k(94, 0, 0), k(92, 0, 0)},
			arm2: []opKey{k(99, 0, 0), k(97, 0, 0), k(95, 0, 0), k(93, 0, 0), k(91, 0, 0)},
		},
		{
			name: "arm1 strictly newer — the whole page comes from one arm",
			arm1: []opKey{k(200, 0, 0), k(199, 0, 0), k(198, 0, 0), k(197, 0, 0), k(196, 0, 0)},
			arm2: []opKey{k(100, 0, 0), k(99, 0, 0), k(98, 0, 0)},
		},
		{
			name: "cross-arm duplicates (sourced AND participant)",
			arm1: []opKey{k(100, 1, 0), k(100, 1, 1), k(99, 0, 0)},
			arm2: []opKey{k(100, 1, 0), k(100, 1, 1), k(98, 0, 0), k(97, 0, 0)},
		},
		{
			name: "both arms shorter than the page",
			arm1: []opKey{k(100, 0, 0)},
			arm2: []opKey{k(99, 0, 0)},
		},
		{
			name: "same ledger, tie-broken by tx_index then op_index",
			arm1: []opKey{k(100, 5, 1), k(100, 5, 0), k(100, 4, 9), k(100, 1, 0)},
			arm2: []opKey{k(100, 5, 2), k(100, 4, 10), k(100, 0, 0)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Pre-fix semantics: unbounded arms, bounded only by the outer merge.
			want := mergeTopN([][]opKey{tc.arm1, tc.arm2}, limit)
			// Post-fix semantics: each arm cut to its own top-N first.
			got := mergeTopN([][]opKey{
				armTopN(tc.arm1, armLimit),
				armTopN(tc.arm2, armLimit),
			}, limit)

			if len(got) != len(want) {
				t.Fatalf("bounded arms returned %d rows, unbounded returned %d — rows lost at the seam\n got: %v\nwant: %v",
					len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("row %d = %v, want %v — bounding the arms changed the merged page\n got: %v\nwant: %v",
						i, got[i], want[i], got, want)
				}
			}
		})
	}
}

// TestAccountOpTypeCounts_QueryShape pins the aggregate variant
// (accountOpTypeCountsQuery, GET /v1/accounts/{g}/activity) to the same
// two-arm discipline as the row listings: sourced arm on the
// source_account skip-index, participant arm resolved on the operations
// primary key — never an `OR … IN (…)` (which defeats the skip-index and
// full-scans the multi-billion-row table), and uniqExact-deduped per arm
// (a plain count() over ReplacingMergeTree inflates on un-merged
// duplicate parts).
func TestAccountOpTypeCounts_QueryShape(t *testing.T) {
	arm1, arm2 := armBodies(t, accountOpTypeCountsQuery)
	if !strings.Contains(arm1, "source_account = ?") {
		t.Errorf("arm 1 must probe the source_account skip-index:\n%s", arm1)
	}
	if !strings.Contains(arm2, "operation_participants WHERE account = ?") {
		t.Errorf("arm 2 must resolve via the account-prefixed participant index:\n%s", arm2)
	}
	for i, arm := range []string{arm1, arm2} {
		if !strings.Contains(arm, "uniqExact((ledger_seq, tx_index, op_index))") {
			t.Errorf("arm %d must uniqExact-dedup the RMT primary key:\n%s", i+1, arm)
		}
		if strings.Contains(arm, " OR ") {
			t.Errorf("arm %d must not OR predicates (skip-index defeat):\n%s", i+1, arm)
		}
	}
}
