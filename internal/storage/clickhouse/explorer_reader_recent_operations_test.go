package clickhouse

import (
	"context"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Regression tests for #444 / #332 F2: RecentOperations carried NO ledger
// lower bound on either arm, so ClickHouse had every part in every partition
// as a candidate for the reverse read. Measured on r1's system.query_log
// (user api_serving, 2026-09-02): one 50-row first page read 10.3M rows /
// 1.37 GiB in 1,788–1,875 ms, which is what put /v1/operations at a 2.238 s
// 6h p95 — the "cheap streamed reverse scan" the query's own comment
// claimed, refuted by the engine's own accounting.
//
// These pin the SQL text through the exported reader (the same
// stubConn/stubRows harness as explorer_reader_dat10_test.go), so they hold
// across a rename of the private builder. They also pin the two-pass
// contract, which is the CORRECTNESS half: a bounded page that comes back
// short must be retried unbounded, never truncated.

// opsPage builds n descending light rows starting at `top`.
func opsPage(top uint32, n int) [][]any {
	out := make([][]any, 0, n)
	for i := range n {
		out = append(out, opLightRowFor(top-uint32(i), 0, 0))
	}
	return out
}

const tailWindowPredicate = "WHERE ledger_seq > (SELECT max(ledger_seq) FROM stellar.operations) - ?"

func TestRecentOperations_FirstPageIsBoundedToTheTailWindow(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) {
		return &stubRows{data: opsPage(63_000_000, 50)}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.RecentOperations(context.Background(), 50, ExplorerCursor{})
	if err != nil {
		t.Fatalf("RecentOperations: %v", err)
	}
	if len(rows) != 50 {
		t.Fatalf("rows = %d, want 50", len(rows))
	}
	// A FULL page must be answered by the bounded read alone — the
	// unbounded fallback exists for short pages only, and firing it here
	// would double every directory read.
	if len(conn.queries) != 1 {
		t.Fatalf("issued %d queries for a full page, want 1 (bounded only): %v", len(conn.queries), conn.queries)
	}
	q := conn.queries[0]
	if !strings.Contains(q, tailWindowPredicate) {
		t.Fatalf("first-page query carries no tail-window lower bound (this is the #444 defect):\n%s", q)
	}
	// The bound must be the window width, and it must be the FIRST arg so
	// it binds to the `- ?` and not to the page-size LIMIT.
	args := conn.args[0]
	if len(args) != 2 {
		t.Fatalf("first-page args = %v, want [window, limit]", args)
	}
	if got, want := args[0], uint32(recentLedgersTailWindow); got != want {
		t.Errorf("window arg = %v (%T), want %v", got, got, want)
	}
	if got, want := args[1], 50; got != want {
		t.Errorf("limit arg = %v, want %v", got, want)
	}
}

func TestRecentOperations_CursorPageIsBoundedBelowTheCursor(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) {
		return &stubRows{data: opsPage(62_999_000, 25)}, nil
	}
	r := &ExplorerReader{conn: conn}

	cur := ExplorerCursor{Ledger: 62_999_100, A: 7, B: 2}
	if _, err := r.RecentOperations(context.Background(), 25, cur); err != nil {
		t.Fatalf("RecentOperations: %v", err)
	}
	if len(conn.queries) != 1 {
		t.Fatalf("issued %d queries for a full cursor page, want 1: %v", len(conn.queries), conn.queries)
	}
	q := conn.queries[0]
	// The keyset comparison still bounds the page from ABOVE — paging must
	// stay exact across a ledger that straddles a page boundary. Since #484
	// it is spelled index-prunably rather than as a bare tuple compare;
	// explorer_reader_deep_cursor_test.go pins that spelling.
	if !strings.Contains(q, recentOperationsCursorPredicate) {
		t.Fatalf("cursor page lost its keyset comparison:\n%s", q)
	}
	if !strings.Contains(q, "AND ledger_seq >= ?") {
		t.Fatalf("cursor page carries no tail-window lower bound (this is the #444 defect):\n%s", q)
	}
	args := conn.args[0]
	want := []any{cur.Ledger, cur.Ledger, cur.A, cur.B, cur.Ledger - uint32(recentLedgersTailWindow), 25}
	if len(args) != len(want) {
		t.Fatalf("cursor args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("arg[%d] = %v (%T), want %v (%T)", i, args[i], args[i], want[i], want[i])
		}
	}
}

// A cursor inside the first `recentLedgersTailWindow` ledgers must clamp its
// lower bound at 0 rather than wrap through uint32 — an underflow would make
// the predicate `ledger_seq >= 4294962296` and return nothing at all.
func TestRecentOperations_CursorNearGenesisClampsTheLowerBound(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) {
		return &stubRows{data: opsPage(900, 10)}, nil
	}
	r := &ExplorerReader{conn: conn}

	if _, err := r.RecentOperations(context.Background(), 10, ExplorerCursor{Ledger: 1000, A: 0, B: 0}); err != nil {
		t.Fatalf("RecentOperations: %v", err)
	}
	args := conn.args[0]
	// args = [ledger, ledger, tx_index, op_index, lower, limit] — the lower
	// bound is index 4 since #484 bound the ledger to both predicate arms.
	if got, want := args[4], uint32(0); got != want {
		t.Fatalf("lower bound near genesis = %v (%T), want %v — uint32 underflow would empty the page", got, got, want)
	}
}

// The correctness half of the bound: a tail window that does not hold a full
// page (a quiet network, or a sparse historical region under a cursor) must
// fall back to the UNBOUNDED read, not serve a short page. A short page here
// would both truncate the listing and mint a next_cursor from the wrong last
// row, permanently skipping every operation between.
func TestRecentOperations_ShortBoundedPageFallsBackToTheUnboundedRead(t *testing.T) {
	conn := &stubConn{}
	full := opsPage(40_000_000, 50)
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, tailWindowPredicate) {
			// The window holds only 3 operations — a quiet network.
			return &stubRows{data: opsPage(40_000_000, 3)}, nil
		}
		return &stubRows{data: full}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.RecentOperations(context.Background(), 50, ExplorerCursor{})
	if err != nil {
		t.Fatalf("RecentOperations: %v", err)
	}
	if len(rows) != 50 {
		t.Fatalf("rows = %d, want 50 — the short bounded page must be replaced by the unbounded read, not served", len(rows))
	}
	if rows[len(rows)-1].Seq != 40_000_000-49 {
		t.Errorf("last row seq = %d, want %d — the returned page must be the UNBOUNDED read's rows", rows[len(rows)-1].Seq, 40_000_000-49)
	}
	if len(conn.queries) != 2 {
		t.Fatalf("issued %d queries, want 2 (bounded then unbounded fallback): %v", len(conn.queries), conn.queries)
	}
	if !strings.Contains(conn.queries[0], tailWindowPredicate) {
		t.Errorf("first query was not the bounded one:\n%s", conn.queries[0])
	}
	if strings.Contains(conn.queries[1], "WHERE") {
		t.Errorf("fallback query must carry no predicate at all on a first page:\n%s", conn.queries[1])
	}
	// The fallback keeps every construct the bounded arm has — losing the
	// DAT-10 dedup or the thread pin on this arm reintroduces those classes
	// on exactly the reads that are already the expensive ones.
	if !strings.Contains(conn.queries[1], "LIMIT 1 BY ledger_seq, tx_index, op_index") {
		t.Errorf("fallback query lost its DAT-10 dedup:\n%s", conn.queries[1])
	}
	requireScanSettings(t, "RecentOperations fallback", conn.queries[1])
}

func TestRecentOperations_ShortCursorPageFallsBackKeepingItsCursorClause(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "AND ledger_seq >= ?") {
			return &stubRows{data: opsPage(30_000_000, 2)}, nil
		}
		return &stubRows{data: opsPage(30_000_000, 20)}, nil
	}
	r := &ExplorerReader{conn: conn}

	cur := ExplorerCursor{Ledger: 30_000_100, A: 1, B: 0}
	rows, err := r.RecentOperations(context.Background(), 20, cur)
	if err != nil {
		t.Fatalf("RecentOperations: %v", err)
	}
	if len(rows) != 20 {
		t.Fatalf("rows = %d, want 20 (the unbounded fallback's page)", len(rows))
	}
	if len(conn.queries) != 2 {
		t.Fatalf("issued %d queries, want 2: %v", len(conn.queries), conn.queries)
	}
	fb, fbArgs := conn.queries[1], conn.args[1]
	if !strings.Contains(fb, recentOperationsCursorPredicate) {
		t.Fatalf("fallback dropped the keyset cursor — it would restart the listing at the tip:\n%s", fb)
	}
	if strings.Contains(fb, "AND ledger_seq >= ?") {
		t.Fatalf("fallback still carries the window bound; it is not a fallback:\n%s", fb)
	}
	want := []any{cur.Ledger, cur.Ledger, cur.A, cur.B, 20}
	if len(fbArgs) != len(want) {
		t.Fatalf("fallback args = %v, want %v", fbArgs, want)
	}
	for i := range want {
		if fbArgs[i] != want[i] {
			t.Errorf("fallback arg[%d] = %v, want %v", i, fbArgs[i], want[i])
		}
	}
}
