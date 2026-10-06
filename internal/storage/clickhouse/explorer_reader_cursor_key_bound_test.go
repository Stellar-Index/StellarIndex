package clickhouse

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

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
		if _, err := (&ExplorerReader{conn: conn}).AccountTransactions(ctx, "GTEST", limit, cur); err != nil {
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
