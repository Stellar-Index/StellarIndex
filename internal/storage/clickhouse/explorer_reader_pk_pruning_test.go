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
// is a primary-key-prefix range read — and touches the wide table only with
// literal key lists: the hydration pass and the participant visibility lookup
// (INV-2697). A wide-table query with `IN (SELECT` is the pathology.
func TestAccountListings_ArmsPageAccountKeyedTables(t *testing.T) {
	cur := ExplorerCursor{Ledger: 63_000_000, A: 4, B: 2}
	for name, tc := range map[string]struct {
		wide, cursor string
		run          func(*ExplorerReader) error
	}{
		"operations": {"FROM stellar.operations", "(ledger_seq, tx_index, op_index) < (?, ?, ?)", func(r *ExplorerReader) error {
			_, err := r.AccountOperations(context.Background(), "GTEST", 9, cur)
			return err
		}},
		"transactions": {"FROM stellar.transactions", "(ledger_seq, tx_index) < (?, ?)", func(r *ExplorerReader) error {
			_, err := r.AccountTransactions(context.Background(), "GTEST", 9, cur)
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

// TestAccountOperations_BoundArgsBindInsideKeyArms guards the exact sourced
// arm's bind order with a cursor and no watermark: no bound placeholder, and
// the args line up with the emitted SQL.
func TestAccountOperations_BoundArgsBindInsideKeyArms(t *testing.T) {
	cur := ExplorerCursor{Ledger: 63_000_000, A: 4, B: 2}
	q, args := exactOpsArm(t, withOpsBySourceRows(func(string) (driver.Rows, error) { return &stubRows{}, nil }), 9, cur)
	if strings.Count(q, "ledger_seq <= ?") != strings.Count(q, "ledger_seq <= ? AND (ledger_seq, tx_index, op_index) < (?, ?, ?)") {
		t.Fatalf("unbounded path must not carry a bound placeholder: %s", q)
	}
	assertArgs(t, q, args, []any{"GTEST", cur.Ledger, cur.Ledger, cur.A, cur.B, 9})
}
