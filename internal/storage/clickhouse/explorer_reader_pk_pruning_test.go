package clickhouse

import (
	"context"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TestAccountListings_ArmsPageAccountKeyedTables pins the 2026-08-28
// rewrite of the account listing arms (r1: `AccountOperations deadline
// exceeded` 503s, 14×/24h, for an account with 11,925 sourced + 26,064
// participant ops).
//
// The old arms resolved their keys OVER THE WIDE TABLE:
//
//	SELECT pk FROM stellar.operations
//	 WHERE pk IN (SELECT pk FROM stellar.ops_by_source WHERE source_account = ?)
//	 ORDER BY pk DESC LIMIT n
//
// ClickHouse's set-based index analysis prunes that to exact granules —
// but to ONE GRANULE PER KEY IN THE SET, before the LIMIT ever applies.
// 37,989 keys × 8192-row granules is the 164–238 M rows / ~8 s per page
// the live query_log showed. The page's cost was the account's whole
// history, not the page size.
//
// The fix pages the ACCOUNT-KEYED tables directly — ops_by_source and
// operation_participants are ORDER BY (account, ledger_seq, tx_index,
// op_index), so `account = ?` + cursor + bound + `ORDER BY … DESC LIMIT n`
// is a primary-key-prefix range read — and touches the wide table ONCE,
// in the hydration pass, over ≤ 3×limit keys. This test asserts that
// shape: the wide table appears exactly once (hydration), never inside
// an arm; each arm reads its key table with the account predicate, the
// cursor and (for operations) the watermark bound applied IN the arm.
// innerArmBodies is armBodies minus the hydration head that precedes the
// first arm (`SELECT <cols> FROM <wide table> WHERE pk IN (SELECT pk FROM (`),
// so arm 1 can be asserted to NOT mention the wide table.
func innerArmBodies(t *testing.T, q string) (string, string) {
	t.Helper()
	arm1, arm2 := armBodies(t, q)
	i := strings.Index(arm1, "(SELECT ledger_seq")
	if i < 0 {
		t.Fatalf("arm 1 has no inner keyset SELECT: %s", arm1)
	}
	return arm1[i:], arm2
}

func TestAccountListings_ArmsPageAccountKeyedTables(t *testing.T) {
	t.Run("operations", func(t *testing.T) {
		q := accountOperationsQuery(true, true)
		arm1, arm2 := innerArmBodies(t, q)
		if n := strings.Count(q, "FROM stellar.operations"); n != 1 {
			t.Errorf("stellar.operations appears %d times, want exactly 1 (the hydration pass) — an arm resolving keys over the wide table reads one granule per account key:\n%s", n, q)
		}
		for i, arm := range []string{arm1, arm2} {
			if strings.Contains(arm, "stellar.operations") {
				t.Errorf("arm %d resolves over stellar.operations — must page its account-keyed table instead:\n%s", i+1, arm)
			}
			if !strings.Contains(arm, "ledger_seq <= ?") {
				t.Errorf("arm %d must apply the watermark bound on the key table:\n%s", i+1, arm)
			}
			if !strings.Contains(arm, "(ledger_seq, tx_index, op_index) < (?, ?, ?)") {
				t.Errorf("arm %d must apply the cursor on the key table:\n%s", i+1, arm)
			}
			if !strings.Contains(arm, "LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?") {
				t.Errorf("arm %d must dedupe (RMT duplicate part) before its LIMIT:\n%s", i+1, arm)
			}
		}
		if !strings.Contains(arm1, "FROM stellar.ops_by_source\n\t\t       WHERE source_account = ? AND op_index != 4294967295") {
			t.Errorf("arm 1 must be a source_account-prefixed read of ops_by_source excluding the tx sentinel:\n%s", arm1)
		}
		if !strings.Contains(arm2, "FROM stellar.operation_participants\n\t\t       WHERE account = ?") {
			t.Errorf("arm 2 must be an account-prefixed read of operation_participants:\n%s", arm2)
		}
	})
	t.Run("transactions", func(t *testing.T) {
		q := accountTransactionsQuery(true)
		arm1, arm2 := innerArmBodies(t, q)
		if n := strings.Count(q, "FROM stellar.transactions"); n != 1 {
			t.Errorf("stellar.transactions appears %d times, want exactly 1 (the hydration pass):\n%s", n, q)
		}
		for i, arm := range []string{arm1, arm2} {
			if strings.Contains(arm, "stellar.transactions") {
				t.Errorf("arm %d resolves over stellar.transactions — must page its account-keyed table instead:\n%s", i+1, arm)
			}
			if !strings.Contains(arm, "(ledger_seq, tx_index) < (?, ?)") {
				t.Errorf("arm %d must apply the cursor on the key table:\n%s", i+1, arm)
			}
			// One key per tx: ops_by_source carries a tx-sentinel row plus
			// one row per op the account sourced, all with the same
			// (ledger_seq, tx_index) — collapse them BEFORE the arm's
			// LIMIT or a 40-op tx eats 41 of the page's slots.
			if !strings.Contains(arm, "LIMIT 1 BY ledger_seq, tx_index LIMIT ?") {
				t.Errorf("arm %d must collapse to one key per tx before its LIMIT:\n%s", i+1, arm)
			}
		}
		if !strings.Contains(arm1, "FROM stellar.ops_by_source\n\t\t       WHERE source_account = ?") {
			t.Errorf("arm 1 must be a source_account-prefixed read of ops_by_source:\n%s", arm1)
		}
		if !strings.Contains(arm2, "FROM stellar.operation_participants\n\t\t       WHERE account = ?") {
			t.Errorf("arm 2 must be an account-prefixed read of operation_participants:\n%s", arm2)
		}
	})
}

// TestAccountOperations_BoundArgsBindInsideKeyArms guards the bind order
// after the rewrite: the arms lost their inner IN-subquery but kept the
// same placeholder sequence (account, bound, cursor×3, limit) per arm, so
// the reader's args slice must still line up with the emitted SQL.
func TestAccountOperations_BoundArgsBindInsideKeyArms(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) { return &stubRows{}, nil }
	r := &ExplorerReader{conn: conn}
	cur := ExplorerCursor{Ledger: 63_000_000, A: 4, B: 2}
	if _, err := r.AccountOperations(context.Background(), "GTEST", 9, cur); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	q := conn.queries[len(conn.queries)-1]
	args := conn.args[len(conn.args)-1]
	// stubConn has no watermark table → unbounded path: no bound placeholder.
	if strings.Contains(q, "ledger_seq <= ?") {
		t.Fatalf("unbounded path must not carry a bound placeholder: %s", q)
	}
	if got, want := strings.Count(q, "?"), len(args); got != want {
		t.Fatalf("query has %d placeholders, reader bound %d args: %v", got, want, args)
	}
	want := []any{"GTEST", uint32(63_000_000), uint32(4), uint32(2), 9, "GTEST", uint32(63_000_000), uint32(4), uint32(2), 9, 9, 9}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("arg %d = %v (%T), want %v (%T)", i, args[i], args[i], want[i], want[i])
		}
	}
}
