package clickhouse

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Regression tests that the UNION arms in AccountOperations and AccountTransactions each carry their own
// ORDER BY / LIMIT; bounding only the outer query materialises every row an active account touched. See
// TestAccountListings_ExactSourcedArmIsBounded and TestUnionArmTopN_MatchesUnboundedMerge.

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

// exactOpsArm runs AccountOperations with a filled sourced window, which
// forces the exact sourced arm, and returns that query and its args. base
// answers every other query.
func exactOpsArm(t *testing.T, base func(string) (driver.Rows, error), limit int, cur ExplorerCursor) (string, []any) {
	t.Helper()
	filled := make([][]any, windowRows(limit, windowFactorKeys))
	for i := range filled {
		filled[i] = keyRow(100, 0, 0)
	}
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		if !isOpsBySourceProbe(q) && strings.Contains(q, "FROM stellar.ops_by_source WHERE source_account") {
			return &stubRows{data: filled}, nil
		}
		return base(q)
	}}
	if _, _, err := (&ExplorerReader{conn: conn}).AccountOperations(context.Background(), "GTEST", limit, cur); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	for i, q := range conn.queries {
		if strings.Contains(q, "LIMIT 1 BY") && strings.Contains(q, "stellar.ops_by_source") {
			return q, conn.args[i]
		}
	}
	t.Fatalf("no exact sourced arm among %v", conn.queries)
	return "", nil
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

// TestUnionArmTopN_MatchesUnboundedMerge: cutting each arm to its own top-N before merging is safe because
// a true top-N row has at most N-1 rows ahead of it within its own arm. It uses the page size read off the
// emitted query, over interleaved, one-sided-newer, duplicate and short arms.
func TestUnionArmTopN_MatchesUnboundedMerge(t *testing.T) {
	const limit = 4

	// Read the per-arm page size out of the query the reader emits, so this
	// property is anchored to the implementation rather than to a constant.
	_, args := exactOpsArm(t, withOpsBySourceRows(func(string) (driver.Rows, error) { return &stubRows{}, nil }), limit, ExplorerCursor{})
	armLimit, ok := args[len(args)-1].(int) // [account, LIMIT]
	if !ok {
		t.Fatalf("exact arm limit arg is %T, want int (args: %v)", args[len(args)-1], args)
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
			// Without per-arm bounds: unbounded arms, bounded only by the outer merge.
			want := mergeTopN([][]opKey{tc.arm1, tc.arm2}, limit)
			// With per-arm bounds: each arm cut to its own top-N first.
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

// TestAccountOpTypeCounts_QueryShape pins accountOpTypeCountsQuery to the two-arm discipline of the row
// listings: sourced arm on the skip-index, participant arm on the operations PK, never `OR ... IN (...)`
// (full scan), and uniqExact per arm (count() over ReplacingMergeTree inflates on unmerged duplicates).
func TestAccountOpTypeCounts_QueryShape(t *testing.T) {
	arm1, arm2 := armBodies(t, accountOpTypeCountsQuery)
	if !strings.Contains(arm1, "source_account = ?") {
		t.Errorf("arm 1 must probe the source_account skip-index:\n%s", arm1)
	}
	if !strings.Contains(arm2, "operation_participants WHERE account = ?") {
		t.Errorf("arm 2 must resolve via the account-prefixed participant index:\n%s", arm2)
	}
	// The participant arm counts only ops of successful txs or txs
	// the account sourced, resolved over the arm's own participant keys.
	if !strings.Contains(arm2, "FROM stellar.transactions") || !strings.Contains(arm2, visibleTxPredicate) {
		t.Errorf("arm 2 must keep only visible txs (%s):\n%s", visibleTxPredicate, arm2)
	}
	for i, arm := range []string{arm1, strings.ReplaceAll(arm2, visibleTxPredicate, "")} {
		if !strings.Contains(arm, "uniqExact((ledger_seq, tx_index, op_index))") {
			t.Errorf("arm %d must uniqExact-dedup the RMT primary key:\n%s", i+1, arm)
		}
		if strings.Contains(arm, " OR ") {
			t.Errorf("arm %d must not OR predicates (skip-index defeat):\n%s", i+1, arm)
		}
	}
}

// Each keyset cursor must carry a redundant leading-key bound beside its
// tuple comparison. KeyCondition does not prune on a multi-column tuple, so
// without the bound a deep page reads every primary-key row above the cursor.
func TestKeysetCursors_CarryLeadingKeyBound(t *testing.T) {
	for name, tc := range map[string]struct {
		q, clause string
		perQuery  int
	}{
		"account transactions": {sourcedTxKeysExactQuery(true), "ledger_seq <= ? AND (ledger_seq, tx_index) < (?, ?)", 1},
		"account operations":   {sourcedOpKeysExactQuery(true, false), "ledger_seq <= ? AND (ledger_seq, tx_index, op_index) < (?, ?, ?)", 1},
		"account operations (watermark)": {
			sourcedOpKeysExactQuery(true, true), "ledger_seq <= ? AND (ledger_seq, tx_index, op_index) < (?, ?, ?)", 1,
		},
		"contract events":       {contractEventsRecentQuery(true, false), "ledger_seq <= ? AND (ledger_seq, tx_hash, op_index, event_index) < (?, ?, ?, ?)", 1},
		"contract events dedup": {contractEventsRecentDedupQuery(true, true), "ledger_seq <= ? AND (ledger_seq, tx_hash, op_index, event_index) < (?, ?, ?, ?)", 1},
		"account movements": {
			accountMovementsQuery(AccountMovementFilter{HasMaxLedger: true}, true),
			"ledger <= ? AND (ledger, tx_hash, op_index, leg_index) < (?, ?, ?, ?)", 1,
		},
	} {
		if got := strings.Count(tc.q, tc.clause); got != tc.perQuery {
			t.Errorf("%s: %d cursor clauses with a leading-key bound, want %d:\n%s", name, got, tc.perQuery, tc.q)
		}
	}
}

// The extra placeholder must bind the cursor's own ledger, in clause order,
// on every reader that issues a cursor page.
func TestKeysetCursors_LeadingKeyBoundBindsCursorLedger(t *testing.T) {
	const limit = 7
	empty := func(string) (driver.Rows, error) { return &stubRows{}, nil }
	ctx := context.Background()

	t.Run("account transactions", func(t *testing.T) {
		conn := &stubConn{respond: withOpsBySourceRows(empty)}
		cur := ExplorerCursor{Ledger: 63_000_000, A: 4}
		if _, _, err := (&ExplorerReader{conn: conn}).AccountTransactions(ctx, "GTEST", limit, cur); err != nil {
			t.Fatalf("AccountTransactions: %v", err)
		}
		// An empty window proves an exhausted arm, so the last query is the
		// participant arm's first window.
		assertBinds(t, conn, []any{"GTEST", cur.Ledger, cur.Ledger, cur.A, windowRows(limit, windowFactorKeys)})
	})
	t.Run("contract events", func(t *testing.T) {
		conn := &stubConn{respond: empty}
		cur := ContractEventsCursor{Ledger: 55_000_000, TxHash: "ab", OpIndex: 1, EventIndex: 2}
		if _, err := (&ExplorerReader{conn: conn}).ContractEventsRecent(ctx, "CTEST", limit, cur); err != nil {
			t.Fatalf("ContractEventsRecent: %v", err)
		}
		assertBinds(t, conn, []any{"CTEST", cur.Ledger, cur.Ledger, cur.TxHash, cur.OpIndex, cur.EventIndex, limit + contractEventsDedupHeadroom})
	})
	t.Run("account movements", func(t *testing.T) {
		conn := &stubConn{respond: empty}
		cur := AccountMovementCursor{Ledger: 40_000_000, TxHash: "cd", OpIndex: 3, LegIndex: 1}
		filter := AccountMovementFilter{Kind: "k", MaxLedger: 41_000_000, HasMaxLedger: true}
		if _, err := (&ExplorerReader{conn: conn}).AccountMovements(ctx, "GTEST", limit, cur, filter); err != nil {
			t.Fatalf("AccountMovements: %v", err)
		}
		assertBinds(t, conn, []any{"GTEST", "k", filter.MaxLedger, cur.Ledger, cur.Ledger, cur.TxHash, cur.OpIndex, cur.LegIndex, windowRows(limit, windowFactorKeys)})
	})
}

func assertBinds(t *testing.T, conn *stubConn, want []any) {
	t.Helper()
	q, got := conn.queries[len(conn.queries)-1], conn.args[len(conn.args)-1]
	if n := strings.Count(q, "?"); n != len(got) {
		t.Fatalf("query has %d placeholders, reader bound %d args: %v\n%s", n, len(got), got, q)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

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

// TestAccountListings_ArmsPageAccountKeyedTables: arms must page ops_by_source and operation_participants
// by primary-key prefix (`account = ?` + cursor + bound), never `pk IN (SELECT ...)` on the wide table,
// which costs the account's whole history per page. The wide table gets literal key lists only.
func TestAccountListings_ArmsPageAccountKeyedTables(t *testing.T) {
	cur := ExplorerCursor{Ledger: 63_000_000, A: 4, B: 2}
	for name, tc := range map[string]struct {
		wide, cursor string
		run          func(*ExplorerReader) error
	}{
		"operations": {"FROM stellar.operations", "(ledger_seq, tx_index, op_index) < (?, ?, ?)", func(r *ExplorerReader) error {
			_, _, err := r.AccountOperations(context.Background(), "GTEST", 9, cur)
			return err
		}},
		"transactions": {"FROM stellar.transactions", "(ledger_seq, tx_index) < (?, ?)", func(r *ExplorerReader) error {
			_, _, err := r.AccountTransactions(context.Background(), "GTEST", 9, cur)
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			conn := &stubConn{}
			conn.respond = withOpsBySourceRows(func(q string) (driver.Rows, error) {
				if strings.Contains(q, "FROM stellar.operation_participants") && !isVisibilityLookup(q) {
					if name == "transactions" {
						return &stubRows{data: [][]any{txKeyRow(62_000_000, 0)}}, nil
					}
					return &stubRows{data: [][]any{keyRow(62_000_000, 0, 0)}}, nil
				}
				return &stubRows{}, nil
			})
			if err := tc.run(&ExplorerReader{conn: conn}); err != nil {
				t.Fatal(err)
			}
			var sourced, participant int
			for _, q := range conn.queries {
				if strings.Contains(q, tc.wide) && strings.Contains(q, "IN (SELECT") {
					t.Errorf("wide table resolved through a subquery — one granule per account key:\n%s", q)
				}
				switch {
				case isOpsBySourceProbe(q) || isVisibilityLookup(q):
				case strings.Contains(q, "FROM stellar.ops_by_source WHERE source_account = ?"):
					sourced++
				case strings.Contains(q, "FROM stellar.operation_participants WHERE account = ?"):
					participant++
				default:
					continue
				}
				if !isOpsBySourceProbe(q) && !isVisibilityLookup(q) && !strings.Contains(q, tc.cursor) {
					t.Errorf("key read must apply the cursor on the key table:\n%s", q)
				}
			}
			if sourced == 0 || participant == 0 {
				t.Errorf("key reads: sourced=%d participant=%d, want both on their account-keyed tables: %v", sourced, participant, conn.queries)
			}
		})
	}
}

// The exact sourced arms must ORDER BY the full sort key and
// dedupe on it BEFORE their own LIMIT, else an un-merged duplicate part eats a
// slot or the account's whole history materialises.
func TestAccountListings_ExactSourcedArmIsBounded(t *testing.T) {
	for name, tc := range map[string]struct{ q, order, dedupe string }{
		"transactions": {sourcedTxKeysExactQuery(true), "ORDER BY ledger_seq DESC, tx_index DESC", "LIMIT 1 BY ledger_seq, tx_index LIMIT ?"},
		"operations": {
			sourcedOpKeysExactQuery(true, true), "ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC",
			"LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?",
		},
	} {
		if !strings.Contains(tc.q, tc.order) || !strings.Contains(tc.q, tc.dedupe) {
			t.Errorf("%s exact arm lost its ORDER BY / LIMIT 1 BY … LIMIT:\n%s", name, tc.q)
		}
	}
	const limit = 37
	_, args := exactOpsArm(t, withOpsBySourceRows(func(string) (driver.Rows, error) { return &stubRows{}, nil }), limit, ExplorerCursor{})
	if got := args[len(args)-1]; got != limit {
		t.Errorf("exact arm page size = %v, want %d — a smaller per-arm limit drops rows at the merge seam", got, limit)
	}
}
