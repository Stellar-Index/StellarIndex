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
		return &stubRows{data: [][]any{opLightRowFor(100, 0, 0)}}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.RecentOperations(context.Background(), 50, ExplorerCursor{})
	if err != nil {
		t.Fatalf("RecentOperations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	// TWO queries since #444 (2026-09-02): the stub answers with one row for
	// a 50-row page, which is the short-page case, so the tail-window pass is
	// followed by the unbounded fallback (see
	// explorer_reader_recent_operations_test.go). The DAT-10 dedup assertions
	// below therefore have to hold on BOTH arms — an arm that loses it serves
	// a re-ingested operation twice just as surely as the old single query
	// would have.
	if len(conn.queries) != 2 {
		t.Fatalf("issued %d queries, want 2 (bounded pass + unbounded fallback on a short page)", len(conn.queries))
	}
	for _, q := range conn.queries {
		// audit DAT-10: without a dedup construct a re-ingested op leaves an
		// un-merged duplicate part and this directory listing serves it twice.
		if !strings.Contains(q, "LIMIT 1 BY ledger_seq, tx_index, op_index") {
			t.Fatalf("query = %q, want a `LIMIT 1 BY ledger_seq, tx_index, op_index` dedup clause", q)
		}
		// NOT FINAL: it would force a merge across every part in the scanned
		// range, which on the fallback arm is still the whole table.
		if strings.Contains(q, "stellar.operations FINAL") {
			t.Fatalf("query = %q, must NOT use FINAL on the reverse directory scan", q)
		}
		// The LIMIT 1 BY clause must come AFTER the existing ORDER BY and
		// BEFORE the page-size LIMIT (ClickHouse clause order), not replace
		// either.
		if !strings.Contains(q, "ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC LIMIT 1 BY") {
			t.Fatalf("query = %q, want LIMIT 1 BY to follow the existing ORDER BY", q)
		}
	}
}

func TestAccountOperations_DedupsPerPrimaryKeyInBothArms(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		return &stubRows{data: [][]any{opRowFor(100, 0, 0)}}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.AccountOperations(context.Background(), "GTEST", 50, ExplorerCursor{})
	if err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	q := conn.queries[len(conn.queries)-1]
	// audit DAT-10: opCols carries body_xdr (KB-scale) so an outer DISTINCT
	// is the wrong tool here — dedup happens on the narrow primary key.
	// THREE clauses since the two-phase rewrite (2026-08-13): one per
	// UNION arm (both now narrow, key-only) plus the hydration pass,
	// which is the only place the wide columns are read.
	if n := strings.Count(q, "LIMIT 1 BY ledger_seq, tx_index, op_index"); n != 3 {
		t.Fatalf("query has %d `LIMIT 1 BY ledger_seq, tx_index, op_index` clauses, want 3 (two arms + hydration): %s", n, q)
	}
	// body_xdr must be read EXACTLY once — carrying it inside both arms
	// is the regression this shape exists to prevent (opColsLight's doc
	// measures that column at ~600ms over a 24B-row table).
	if n := strings.Count(q, "body_xdr"); n != 1 {
		t.Fatalf("body_xdr appears %d times, want 1 (hydrate once, never per arm): %s", n, q)
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
