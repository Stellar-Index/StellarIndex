package clickhouse

import (
	"context"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

func ident(k int) int { return k }

func TestDedupWindow(t *testing.T) {
	t.Run("short window is exhaustive and collapses adjacent duplicates", func(t *testing.T) {
		got, ok := dedupWindow([]int{9, 9, 8, 7, 7, 7}, 100, 3, ident, nil)
		if !ok || len(got) != 3 || got[0] != 9 || got[1] != 8 || got[2] != 7 {
			t.Fatalf("got %v ok=%v, want [9 8 7] ok", got, ok)
		}
	})
	t.Run("short window with fewer keys than the limit is a real short page", func(t *testing.T) {
		if got, ok := dedupWindow([]int{5, 5}, 100, 3, ident, nil); !ok || len(got) != 1 {
			t.Fatalf("got %v ok=%v, want one key and ok", got, ok)
		}
	})
	t.Run("filled window drops the possibly-incomplete last group", func(t *testing.T) {
		got, ok := dedupWindow([]int{9, 8, 7, 6}, 4, 3, ident, nil)
		if !ok || len(got) != 3 || got[2] != 7 {
			t.Fatalf("got %v ok=%v, want [9 8 7] ok", got, ok)
		}
	})
	t.Run("filled window without enough complete groups is unproven", func(t *testing.T) {
		if _, ok := dedupWindow([]int{9, 9, 9, 9}, 4, 2, ident, nil); ok {
			t.Fatal("one key filling the window proves nothing about the keys below it")
		}
		if _, ok := dedupWindow([]int{9, 8, 8, 8}, 4, 2, ident, nil); ok {
			t.Fatal("one complete group after dropping the last is below limit 2")
		}
	})
	t.Run("newer picks the surviving duplicate regardless of order", func(t *testing.T) {
		type v struct{ k, ver int }
		newer := func(a, b v) bool { return a.ver > b.ver }
		for _, in := range [][]v{{{1, 1}, {1, 2}}, {{1, 2}, {1, 1}}} {
			got, ok := dedupWindow(in, 100, 1, func(x v) int { return x.k }, newer)
			if !ok || len(got) != 1 || got[0].ver != 2 {
				t.Fatalf("got %v ok=%v, want version 2", got, ok)
			}
		}
	})
}

// isVisibilityLookup reports the participant arm's tx-visibility lookup.
func isVisibilityLookup(q string) bool {
	return strings.Contains(q, visibleTxPredicate) && !strings.Contains(q, "uniqExact")
}

// inTuples parses the literal key tuples of a query's `IN ((a,b),…)` list.
func inTuples(q string) [][]uint32 {
	var out [][]uint32
	for _, m := range tupleRE.FindAllStringSubmatch(q, -1) {
		var t []uint32
		for _, f := range strings.Split(m[1], ",") {
			n, err := strconv.ParseUint(f, 10, 32)
			if err != nil {
				panic(err)
			}
			t = append(t, uint32(n))
		}
		out = append(out, t)
	}
	return out
}

var tupleRE = regexp.MustCompile(`\((\d+(?:,\d+)+)\)`)

// visibleTxKeys answers a visibility lookup the way stellar.transactions
// would: only for the tx keys the query sent, minus the failed ones.
func visibleTxKeys(q string, failed map[[2]uint32]bool) *stubRows {
	var out [][]any
	for _, t := range inTuples(q) {
		if !failed[[2]uint32{t[0], t[1]}] {
			out = append(out, []any{t[0], t[1]})
		}
	}
	return &stubRows{data: out}
}

// opsArmRouter answers the account-operations key arms and the hydration read.
type opsArmRouter struct {
	sourced, participant [][]any
	hydrated             [][]any
	failed               map[[2]uint32]bool
}

func (a *opsArmRouter) respond(q string) (driver.Rows, error) {
	switch {
	case isVisibilityLookup(q):
		return visibleTxKeys(q, a.failed), nil
	case strings.Contains(q, "FROM stellar.ops_by_source WHERE source_account"):
		return &stubRows{data: a.sourced}, nil
	case strings.Contains(q, "FROM stellar.operation_participants WHERE account"):
		return &stubRows{data: a.participant}, nil
	case strings.Contains(q, "FROM stellar.operations FINAL"):
		return &stubRows{data: a.hydrated}, nil
	}
	return &stubRows{}, nil
}

func keyRow(ledger, tx, op uint32) []any { return []any{ledger, tx, op} }

func TestAccountOperations_WindowedReadHasNoLimit1By(t *testing.T) {
	const limit = 3
	router := &opsArmRouter{
		sourced:     [][]any{keyRow(100, 0, 0), keyRow(100, 0, 0), keyRow(98, 0, 0)},
		participant: [][]any{keyRow(99, 0, 0), keyRow(97, 0, 0)},
		hydrated:    [][]any{opRowFor(100, 0, 0), opRowFor(99, 0, 0), opRowFor(98, 0, 0)},
	}
	conn := &stubConn{respond: withOpsBySourceRows(router.respond)}
	r := &ExplorerReader{conn: conn}

	rows, _, err := r.AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{Ledger: 200, A: 1, B: 2})
	if err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	var arms, hydrations int
	for i, q := range conn.queries {
		if isOpsBySourceProbe(q) || strings.Contains(q, "account_activity") || isVisibilityLookup(q) {
			continue
		}
		if strings.Contains(q, "LIMIT 1 BY") {
			t.Fatalf("a windowed read carries LIMIT 1 BY (defeats read-in-order): %s", q)
		}
		switch {
		case strings.Contains(q, "FROM stellar.operations FINAL"):
			hydrations++
			if !strings.Contains(q, "IN ((100,0,0),(99,0,0),(98,0,0))") {
				t.Errorf("hydration is not keyed on the merged, deduped page: %s", q)
			}
		default:
			arms++
			want := []any{"GTEST", uint32(200), uint32(200), uint32(1), uint32(2), windowRows(limit, windowFactorKeys)}
			got := conn.args[i]
			if len(got) != len(want) {
				t.Fatalf("arm args = %v, want %v", got, want)
			}
			for j := range want {
				if got[j] != want[j] {
					t.Fatalf("arm arg %d = %v, want %v", j, got[j], want[j])
				}
			}
		}
	}
	if arms != 2 || hydrations != 1 {
		t.Fatalf("arms=%d hydrations=%d, want 2 and 1: %v", arms, hydrations, conn.queries)
	}
}

// A filled window that holds fewer than `limit` complete keys cannot prove the
// page; the reader must run the exact query rather than serve a short page.
func TestAccountOperations_UnprovenWindowFallsBackToExactQuery(t *testing.T) {
	const limit = 3
	window := windowRows(limit, windowFactorKeys)
	filled := make([][]any, window)
	for i := range filled {
		filled[i] = keyRow(100, 0, 0)
	}
	router := &opsArmRouter{sourced: filled, hydrated: [][]any{opRowFor(100, 0, 0)}}
	conn := &stubConn{respond: withOpsBySourceRows(router.respond)}
	r := &ExplorerReader{conn: conn}

	if _, _, err := r.AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	if exactQueries(conn, sourcedOpKeysExactQuery(false, false)) != 1 {
		t.Fatalf("the sourced arm did not fall back to its exact LIMIT 1 BY query: %v", conn.queries)
	}
}

// exactQueries counts the emitted queries equal to the exact form q.
func exactQueries(conn *stubConn, q string) int {
	n := 0
	for _, got := range conn.queries {
		if got == q {
			n++
		}
	}
	return n
}

// txArmRouter answers the account-transactions key arms and hydration.
type txArmRouter struct {
	sourced, participant [][]any
	failed               map[[2]uint32]bool
}

func (a *txArmRouter) respond(q string) (driver.Rows, error) {
	switch {
	case isVisibilityLookup(q):
		return visibleTxKeys(q, a.failed), nil
	case strings.Contains(q, "FROM stellar.ops_by_source WHERE source_account"):
		return &stubRows{data: a.sourced}, nil
	case strings.Contains(q, "FROM stellar.operation_participants WHERE account"):
		return &stubRows{data: a.participant}, nil
	case strings.Contains(q, "FROM stellar.transactions FINAL"):
		return &stubRows{data: [][]any{txRowFor(100, testTxHash)}}, nil
	}
	return &stubRows{}, nil
}

func txKeyRow(ledger, tx uint32) []any { return []any{ledger, tx} }

// The page is full only if cross-arm overlap and the per-tx duplicate rows
// (tx sentinel + one per op) collapse before the page is cut.
func TestAccountTransactions_WindowedMergeCollapsesDuplicatesAndOverlap(t *testing.T) {
	const limit = 3
	router := &txArmRouter{
		sourced:     [][]any{txKeyRow(100, 1), txKeyRow(100, 1), txKeyRow(100, 1), txKeyRow(98, 0)},
		participant: [][]any{txKeyRow(100, 1), txKeyRow(99, 0), txKeyRow(97, 0)},
	}
	conn := &stubConn{respond: withOpsBySourceRows(router.respond)}
	r := &ExplorerReader{conn: conn}

	if _, _, err := r.AccountTransactions(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountTransactions: %v", err)
	}
	last := conn.queries[len(conn.queries)-1]
	if !strings.Contains(last, "IN ((100,1),(99,0),(98,0))") {
		t.Fatalf("hydration keys are not the merged distinct top %d: %s", limit, last)
	}
	for _, q := range conn.queries {
		if strings.Contains(q, "LIMIT 1 BY") {
			t.Fatalf("a windowed read carries LIMIT 1 BY: %s", q)
		}
	}
}

func TestAccountTransactions_UnprovenWindowFallsBackToExactQuery(t *testing.T) {
	const limit = 3
	filled := make([][]any, windowRows(limit, windowFactorTxArm))
	for i := range filled {
		filled[i] = txKeyRow(100, 1) // one 100-op tx fills the window
	}
	router := &txArmRouter{sourced: filled}
	conn := &stubConn{respond: withOpsBySourceRows(router.respond)}
	r := &ExplorerReader{conn: conn}

	if _, _, err := r.AccountTransactions(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountTransactions: %v", err)
	}
	if exactQueries(conn, sourcedTxKeysExactQuery(false)) != 1 {
		t.Fatalf("the sourced arm did not fall back to its exact LIMIT 1 BY query: %v", conn.queries)
	}
}

func movementRow(ledger uint32, counterparty string, ingested time.Time) []any {
	return []any{
		ledger, time.Unix(1700000000, 0).UTC(), "tx", uint32(0), uint32(0), "sent",
		"payment", "classic_derived", "native", counterparty, big.NewInt(1), "{}", ingested,
	}
}

func TestAccountMovements_WindowKeepsNewestVersionPerKey(t *testing.T) {
	old := time.Unix(1_000, 0).UTC()
	fresh := time.Unix(2_000, 0).UTC()
	for _, tc := range []struct {
		name string
		data [][]any
	}{
		{"newer first", [][]any{movementRow(100, "fresh", fresh), movementRow(100, "stale", old)}},
		{"newer last", [][]any{movementRow(100, "stale", old), movementRow(100, "fresh", fresh)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &stubConn{respond: func(q string) (driver.Rows, error) {
				if strings.Contains(q, "LIMIT 1 BY") {
					t.Fatalf("windowed movements read carries LIMIT 1 BY: %s", q)
				}
				return &stubRows{data: tc.data}, nil
			}}
			r := &ExplorerReader{conn: conn}
			rows, err := r.AccountMovements(context.Background(), "GADDR", 10, AccountMovementCursor{}, AccountMovementFilter{})
			if err != nil {
				t.Fatalf("AccountMovements: %v", err)
			}
			if len(rows) != 1 || rows[0].Counterparty != "fresh" {
				t.Fatalf("rows = %+v, want one row carrying the newer version", rows)
			}
		})
	}
}

func TestAccountMovements_UnprovenWindowFallsBackToExactQuery(t *testing.T) {
	const limit = 3
	filled := make([][]any, windowRows(limit, windowFactorKeys))
	for i := range filled {
		filled[i] = movementRow(100, "c", time.Unix(1_000, 0).UTC())
	}
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		if strings.Contains(q, "LIMIT 1 BY") {
			return &stubRows{}, nil
		}
		return &stubRows{data: filled}, nil
	}}
	r := &ExplorerReader{conn: conn}
	if _, err := r.AccountMovements(context.Background(), "GADDR", limit, AccountMovementCursor{}, AccountMovementFilter{}); err != nil {
		t.Fatalf("AccountMovements: %v", err)
	}
	if len(conn.queries) != 2 || !strings.Contains(conn.queries[1], "LIMIT 1 BY ledger, tx_hash, op_index, leg_index") {
		t.Fatalf("want windowed read then the exact query, got %v", conn.queries)
	}
}
