package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Regression tests for audit DAT-10 (ClickHouse ReplacingMergeTree reads that
// neither FINAL nor dedup, over-counting un-merged duplicate rows). These use
// the stubConn/stubRows harness from tx_hash_index_test.go. stubConn does not
// implement real ReplacingMergeTree semantics, so these are query-SHAPE
// assertions — proof that the emitted SQL carries the dedup construct — not
// a live-ClickHouse proof of the merge behavior itself (see the fixer's
// report for that caveat).

// opLightRowFor builds the 7-column opColsLight row scanOpsLight expects.
func opLightRowFor(seq uint32, txIndex, opIndex uint32) []any {
	return []any{seq, time.Unix(1700000000, 0).UTC(), "hash" + string(rune('0'+opIndex)), txIndex, opIndex, "OperationTypePayment", "GSOURCE"}
}

// opRowFor builds the 8-column opCols row scanOps expects (opColsLight +
// body_xdr).
func opRowFor(seq uint32, txIndex, opIndex uint32) []any {
	return []any{seq, time.Unix(1700000000, 0).UTC(), "hash" + string(rune('0'+opIndex)), txIndex, opIndex, "OperationTypePayment", "GSOURCE", "AAAAAA=="}
}

func TestRecentOperations_DedupsPerPrimaryKey(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if !strings.Contains(q, "FROM stellar.operations") {
			t.Fatalf("unexpected query: %s", q)
		}
		// The same operation twice: an un-merged re-ingest duplicate, adjacent
		// in sort-key order.
		return &stubRows{data: [][]any{opLightRowFor(100, 0, 0), opLightRowFor(100, 0, 0)}}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.RecentOperations(context.Background(), 50, ExplorerCursor{})
	if err != nil {
		t.Fatalf("RecentOperations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (the duplicate must collapse in Go)", len(rows))
	}
	// The stub answers a short page, so the tail-window pass is followed by
	// the unbounded fallback; both are windowed reads.
	if len(conn.queries) != 2 {
		t.Fatalf("issued %d queries, want 2 (bounded pass + unbounded fallback on a short page)", len(conn.queries))
	}
	for _, q := range conn.queries {
		// LIMIT 1 BY between ORDER BY and LIMIT disables the reverse
		// read-in-order early exit; the windowed read must not carry it.
		if strings.Contains(q, "LIMIT 1 BY") {
			t.Fatalf("windowed query carries LIMIT 1 BY: %q", q)
		}
		// NOT FINAL: it would force a merge across every part in the scanned
		// range, which on the fallback arm is still the whole table.
		if strings.Contains(q, "stellar.operations FINAL") {
			t.Fatalf("query = %q, must NOT use FINAL on the reverse directory scan", q)
		}
		if !strings.Contains(q, "ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC LIMIT ?") {
			t.Fatalf("query = %q, want a sort-key ORDER BY directly before LIMIT", q)
		}
	}
}

// audit DAT-10: stellar.operations is ReplacingMergeTree, so an account
// listing collapses un-merged duplicate parts on the narrow primary key —
// never with a DISTINCT over opCols, which carries the KB-scale body_xdr. The
// exact sourced arm uses LIMIT 1 BY; the windowed reads collapse adjacent keys
// in Go; body_xdr is read exactly once, by the FINAL hydration (opColsLight's
// doc measures that column at ~600ms over a 24B-row table).
func TestAccountOperations_DedupsOnThePrimaryKey(t *testing.T) {
	const limit = 3
	filled := make([][]any, windowRows(limit, windowFactorKeys))
	for i := range filled {
		filled[i] = keyRow(100, 0, 0) // forces the exact sourced arm
	}
	router := &opsArmRouter{sourced: filled, hydrated: [][]any{opRowFor(100, 0, 0)}}
	conn := &stubConn{respond: withOpsBySourceRows(func(q string) (driver.Rows, error) {
		if strings.Contains(q, "LIMIT 1 BY") {
			return &stubRows{data: [][]any{keyRow(100, 0, 0)}}, nil
		}
		return router.respond(q)
	})}
	rows, err := (&ExplorerReader{conn: conn}).AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{})
	if err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	var exact, wide int
	for _, q := range conn.queries {
		if strings.Contains(q, "LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?") {
			exact++
		}
		if strings.Contains(q, "body_xdr") {
			wide++
			if !strings.Contains(q, "FROM stellar.operations FINAL") {
				t.Errorf("body_xdr read outside the FINAL hydration: %s", q)
			}
		}
		if strings.Contains(q, "DISTINCT") {
			t.Errorf("an account-operations read dedups with DISTINCT: %s", q)
		}
	}
	if exact != 1 || wide != 1 {
		t.Fatalf("exact LIMIT 1 BY reads = %d, body_xdr reads = %d; want 1 and 1: %v", exact, wide, conn.queries)
	}
}

func TestOperationsByTx_UsesFinal(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		return &stubRows{data: [][]any{opRowFor(100, 3, 0)}}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.OperationsByTx(context.Background(), 100, "abchash")
	if err != nil {
		t.Fatalf("OperationsByTx: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	q := conn.queries[len(conn.queries)-1]
	// audit DAT-10: ledger+tx_hash-scoped so FINAL stays cheap (partition +
	// primary-key-prefix bounded), matching the sibling OperationsByLedger.
	if !strings.Contains(q, "FROM stellar.operations FINAL") {
		t.Fatalf("query = %q, want `FROM stellar.operations FINAL`", q)
	}
}
