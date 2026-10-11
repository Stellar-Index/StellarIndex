package clickhouse

import (
	"context"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TestAccountListings_ArmsPageAccountKeyedTables pins the account listing arms against the
// wide-table pathology:
//
//	SELECT pk FROM stellar.operations
//	 WHERE pk IN (SELECT pk FROM stellar.ops_by_source WHERE source_account = ?)
//	 ORDER BY pk DESC LIMIT n
//
// ClickHouse prunes that to one granule per key in the set before the LIMIT applies, so a page
// costs the account's whole history (hundreds of millions of rows), not the page size.
//
// The arms must instead page the ACCOUNT-KEYED tables directly: ops_by_source and
// operation_participants are ORDER BY (account, ledger_seq, tx_index, op_index), so
// `account = ?` + cursor + bound + `ORDER BY … DESC LIMIT n` is a primary-key-prefix range read.
// The wide table is touched only with literal key lists (hydration, participant visibility);
// a wide-table query with `IN (SELECT` is the pathology.
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
