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
// These use the stubConn/stubRows harness from tx_hash_index_test.go.
// TestAccountListings_ExactSourcedArmIsBounded pins the bounded shape;
// TestUnionArmTopN_MatchesUnboundedMerge proves bounding each arm cannot lose
// a row, against the per-arm limit the reader actually binds.

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

// The exact sourced arms (audit C-F1a) must ORDER BY the full sort key and
// dedupe on it BEFORE their own LIMIT, else an un-merged duplicate part eats a
// slot (DAT-10) or the account's whole history materialises.
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
	// INV-2697: the participant arm counts only ops of successful txs or txs
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
