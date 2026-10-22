package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Regression: RecentOperations needs a ledger lower bound on both arms, else a 50-row first page scanned ~10M
// rows. These pin the SQL through the exported reader (stubConn/stubRows from tx_hash_index_test.go)
// and the correctness half: a bounded page that comes back short must be retried unbounded, never truncated.

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
	if got, want := args[1], windowRows(50, windowFactorKeys); got != want {
		t.Errorf("row-budget arg = %v, want the dedup window %v", got, want)
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
	// The keyset comparison must still bound the page from above, so paging stays exact across a ledger
	// straddling a page boundary. It is spelled index-prunably; explorer_reader_union_bounds_test.go pins that
	// spelling.
	if !strings.Contains(q, recentOperationsCursorPredicate) {
		t.Fatalf("cursor page lost its keyset comparison:\n%s", q)
	}
	if !strings.Contains(q, "AND ledger_seq >= ?") {
		t.Fatalf("cursor page carries no tail-window lower bound (this is the #444 defect):\n%s", q)
	}
	// The ledger binds to both keyset arms; a 3-arg tuple shape would bind
	// tx_index to `ledger_seq = ?` and silently return a different page.
	args := conn.args[0]
	want := []any{cur.Ledger, cur.Ledger, cur.A, cur.B, cur.Ledger - uint32(recentLedgersTailWindow), windowRows(25, windowFactorKeys)}
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
	// bound is index 4 because the ledger bounds both predicate arms.
	if got, want := args[4], uint32(0); got != want {
		t.Fatalf("lower bound near genesis = %v (%T), want %v — uint32 underflow would empty the page", got, got, want)
	}
}

// Correctness half of the bound: a tail window that lacks a full page must fall back to the UNBOUNDED read,
// since a short page would truncate the listing and mint next_cursor from the wrong row, skipping operations.
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
	// The thread pin must survive on the unbounded arm — it is already the
	// expensive read. Dedup is done in Go on the window, so the SQL must stay
	// free of LIMIT 1 BY (it defeats read-in-order early exit).
	if strings.Contains(conn.queries[1], "LIMIT 1 BY") {
		t.Errorf("windowed fallback carries LIMIT 1 BY:\n%s", conn.queries[1])
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
	want := []any{cur.Ledger, cur.Ledger, cur.A, cur.B, windowRows(20, windowFactorKeys)}
	if len(fbArgs) != len(want) {
		t.Fatalf("fallback args = %v, want %v", fbArgs, want)
	}
	for i := range want {
		if fbArgs[i] != want[i] {
			t.Errorf("fallback arg[%d] = %v, want %v", i, fbArgs[i], want[i])
		}
	}
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

// A refused cursor must arrive as its OWN class. Without this it is
// indistinguishable from a lake fault, and the route's error mapping cannot
// tell "you asked for an unservable position" from "we broke".
func TestRecentOperations_RefusedCursorIsItsOwnErrorClass(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) {
		return nil, &clickhouse.Exception{
			Code: 158, Name: "TOO_MANY_ROWS",
			Message: "Limit for rows or bytes to read exceeded, max rows: 200.00 million, current rows: 233.82 million",
		}
	}
	r := &ExplorerReader{conn: conn}

	_, err := r.RecentOperations(context.Background(), 50, ExplorerCursor{Ledger: 64_200_000, A: 0, B: 0})
	if err == nil {
		t.Fatal("a refused cursor returned no error — the caller would serve an empty page as if history ended")
	}
	if !errors.Is(err, ErrOperationsCursorTooDeep) {
		t.Fatalf("refused cursor error = %v, want it to wrap ErrOperationsCursorTooDeep", err)
	}
	// The offending cursor must be in the message: an operator reading the
	// log has to know which position was refused.
	if !strings.Contains(err.Error(), "64200000.0.0") {
		t.Errorf("refused-cursor error does not name the cursor: %v", err)
	}
	// Every OTHER server error keeps its existing class — a refusal-shaped
	// classifier that swallowed real faults would be worse than the defect.
	conn2 := &stubConn{}
	conn2.respond = func(string) (driver.Rows, error) {
		return nil, &clickhouse.Exception{Code: 241, Name: "MEMORY_LIMIT_EXCEEDED"}
	}
	r2 := &ExplorerReader{conn: conn2}
	_, err = r2.RecentOperations(context.Background(), 50, ExplorerCursor{Ledger: 64_200_000})
	if err == nil || errors.Is(err, ErrOperationsCursorTooDeep) {
		t.Errorf("a memory-limit failure was misclassified as a cursor refusal: %v", err)
	}
}

// A type-filtered page cannot prune on op_type (no skip index), so it must
// stay inside one tail window below its anchor and never take the unbounded
// fallback RecentOperations uses for a short page.
func TestRecentOperationsOfType_FirstPageIsScopedBelowTheTip(t *testing.T) {
	const tip = uint32(63_000_000)
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.HasPrefix(q, "SELECT max(ledger_seq)") {
			return &stubRows{data: [][]any{{tip}}}, nil
		}
		return &stubRows{data: opsPage(tip, 3)}, nil // short page
	}
	r := &ExplorerReader{conn: conn}

	page, err := r.RecentOperationsOfType(context.Background(), 50, ExplorerCursor{}, []string{"OperationTypePayment"})
	if err != nil {
		t.Fatalf("RecentOperationsOfType: %v", err)
	}
	if len(page.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(page.Rows))
	}
	if want := tip + 1 - uint32(recentLedgersTailWindow); page.ScannedFrom != want {
		t.Errorf("ScannedFrom = %d, want %d", page.ScannedFrom, want)
	}
	if len(conn.queries) != 2 {
		t.Fatalf("issued %d queries, want tip + one bounded page (no unbounded fallback): %v", len(conn.queries), conn.queries)
	}
	q := conn.queries[1]
	for _, frag := range []string{recentOperationsCursorPredicate, "ledger_seq >= ?", "op_type IN (?)", "max_rows_to_read"} {
		if !strings.Contains(q, frag) {
			t.Errorf("page query missing %q:\n%s", frag, q)
		}
	}
	want := []any{
		tip + 1, tip + 1, uint32(0), uint32(0), tip + 1 - uint32(recentLedgersTailWindow),
		[]string{"OperationTypePayment"},
		windowRows(50, windowFactorKeys),
	}
	if !reflect.DeepEqual(conn.args[1], want) {
		t.Errorf("page args = %v, want %v", conn.args[1], want)
	}
}

func TestRecentOperationsOfType_FloorClampsAtGenesis(t *testing.T) {
	conn := &stubConn{respond: func(string) (driver.Rows, error) { return &stubRows{}, nil }}
	r := &ExplorerReader{conn: conn}
	page, err := r.RecentOperationsOfType(context.Background(), 50, ExplorerCursor{Ledger: 100}, []string{"OperationTypeClawback"})
	if err != nil {
		t.Fatalf("RecentOperationsOfType: %v", err)
	}
	if page.ScannedFrom != 0 {
		t.Errorf("ScannedFrom = %d, want 0 (scan reached genesis)", page.ScannedFrom)
	}
	if len(conn.queries) != 1 {
		t.Errorf("a cursor page issued %d queries, want 1", len(conn.queries))
	}
}

func TestRecentOperationsOfType_RefusesEmptyTypeList(t *testing.T) {
	r := &ExplorerReader{conn: &stubConn{}}
	if _, err := r.RecentOperationsOfType(context.Background(), 50, ExplorerCursor{}, nil); err == nil {
		t.Fatal("an empty type list must be refused, not served as an unfiltered scan")
	}
}

// participantLake is one account's stellar.operation_participants rows plus the failed transactions among
// them. It honours the reads' cursor and LIMIT and answers visibility lookups only for keys sent, so
// participantKeys' paging and chunking run for real.
type participantLake struct {
	driver.Conn
	rows    []accountOpKey // newest first; a key may repeat (un-merged parts)
	failed  map[accountTxKey]bool
	lookups []string
	reads   int
}

func (l *participantLake) Query(_ context.Context, q string, args ...any) (driver.Rows, error) {
	if isVisibilityLookup(q) {
		l.lookups = append(l.lookups, q)
		if len(args) != 1 || args[0] != "GTEST" {
			return nil, fmt.Errorf("lookup must bind only the account, got %v", args)
		}
		failed := map[[2]uint32]bool{}
		for k := range l.failed {
			failed[[2]uint32{k.ledger, k.txIndex}] = true
		}
		return visibleTxKeys(q, failed), nil
	}
	if !strings.Contains(q, "FROM stellar.operation_participants WHERE account = ?") || strings.Contains(q, "LIMIT 1 BY") {
		return nil, fmt.Errorf("unexpected query: %s", q)
	}
	l.reads++
	window := args[len(args)-1].(int)
	var from *accountOpKey
	if len(args) == 6 {
		from = &accountOpKey{args[2].(uint32), args[3].(uint32), args[4].(uint32)}
	}
	var out [][]any
	for _, k := range l.rows {
		if from != nil && !from.after(k) {
			continue
		}
		if len(out) == window {
			break
		}
		out = append(out, []any{k.ledger, k.txIndex, k.opIndex})
	}
	return &stubRows{data: out}, nil
}

func (l *participantLake) queries() int { return l.reads + len(l.lookups) }

// walkParticipantKeys pages the participant arm to its end the way the
// reader does: from the scan frontier when the budget ran out, else from the
// last key of a full page. Every call must stay within the query budget.
func walkParticipantKeys(t *testing.T, lake *participantLake, limit int) (got []accountOpKey, frontiers int) {
	t.Helper()
	var from *accountOpKey
	for page := 0; ; page++ {
		before := lake.queries()
		keys, frontier, err := participantKeys(context.Background(), lake, "GTEST", limit, opParticipantArm, "", nil, from)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if n := lake.queries() - before; n > participantQueryBudget {
			t.Fatalf("page %d issued %d queries, budget %d", page, n, participantQueryBudget)
		}
		got = append(got, keys...)
		switch {
		case frontier != nil:
			if len(keys) >= limit {
				t.Fatalf("page %d: a full page must not carry a frontier", page)
			}
			if len(keys) > 0 && !keys[len(keys)-1].after(*frontier) {
				t.Fatalf("page %d: frontier %v is not older than the last key %v", page, *frontier, keys[len(keys)-1])
			}
			frontiers++
			from = frontier
		case len(keys) < limit:
			return got, frontiers
		default:
			from = &keys[len(keys)-1]
		}
	}
}

// TestParticipantKeys_ExactPastFailedTxs walks the participant arm page by page and requires exactly the
// visible keys in order, every page but the last full: a failed tx must not appear or shorten a page.
func TestParticipantKeys_ExactPastFailedTxs(t *testing.T) {
	lake := &participantLake{failed: map[accountTxKey]bool{}}
	var want []accountOpKey
	for ledger := uint32(500); ledger > 100; ledger-- {
		k := accountOpKey{ledger, 1, 0}
		lake.rows = append(lake.rows, k)
		if ledger%7 == 0 {
			lake.rows = append(lake.rows, k) // un-merged duplicate part
		}
		// A long failed run (whole windows) plus scattered failures.
		if (ledger > 300 && ledger <= 420) || ledger%5 == 0 {
			lake.failed[accountTxKey{ledger, 1}] = true
			continue
		}
		want = append(want, k)
	}

	for _, limit := range []int{1, 3, 50, 200} {
		t.Run(fmt.Sprint("limit=", limit), func(t *testing.T) {
			got, frontiers := walkParticipantKeys(t, lake, limit)
			if frontiers != 0 {
				t.Fatalf("a 120-row failed run exhausted the budget %d times", frontiers)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("walk returned %d keys, want %d visible keys in order\n got: %v\nwant: %v", len(got), len(want), got, want)
			}
		})
	}
	for _, q := range lake.lookups {
		if strings.Contains(q, "IN (SELECT") || strings.Contains(q, "IN (\n") {
			t.Fatalf("visibility lookup must be a literal key list, not a subquery: %s", q)
		}
	}
}

// A run of failed txs too long for one request's budget ends the page short at a scan frontier; paging on
// from it visits every key exactly once and returns exactly the visible ones. Lookups stay chunked at a
// fixed size.
func TestParticipantKeys_BudgetEndsPageAtFrontier(t *testing.T) {
	const (
		limit  = 50
		failed = 10000
	)
	lake := &participantLake{failed: map[accountTxKey]bool{}}
	var want []accountOpKey
	ledger := uint32(failed + limit + 10)
	for i := 0; i < limit-1; i++ {
		k := accountOpKey{ledger, 1, 0}
		lake.rows = append(lake.rows, k)
		want = append(want, k)
		ledger--
	}
	for i := 0; i < failed; i++ {
		lake.rows = append(lake.rows, accountOpKey{ledger, 1, 0})
		lake.failed[accountTxKey{ledger, 1}] = true
		ledger--
	}
	last := accountOpKey{ledger, 1, 0}
	lake.rows = append(lake.rows, last)
	want = append(want, last)

	first, frontier, err := participantKeys(context.Background(), lake, "GTEST", limit, opParticipantArm, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if frontier == nil || fmt.Sprint(first) != fmt.Sprint(want[:limit-1]) {
		t.Fatalf("first page: want the %d visible keys and a frontier, got %d keys, frontier %v", limit-1, len(first), frontier)
	}
	if !lake.failed[accountTxKey{frontier.ledger, frontier.txIndex}] {
		t.Fatalf("frontier %v must be the oldest key scanned, inside the failed run", *frontier)
	}

	lake.lookups, lake.reads = nil, 0
	got, frontiers := walkParticipantKeys(t, lake, limit)
	if frontiers == 0 {
		t.Fatal("the walk never exhausted the budget")
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("want the %d visible keys in order, got %d: %v", len(want), len(got), got)
	}
	seen := map[string]bool{}
	for _, q := range lake.lookups {
		for _, k := range inTuples(q) {
			if seen[fmt.Sprint(k)] {
				t.Fatalf("tx %v looked up twice: a window re-read or a frontier overlap", k)
			}
			seen[fmt.Sprint(k)] = true
		}
	}
	if len(seen) != failed+limit {
		t.Fatalf("looked up %d txs, want every one of %d exactly once", len(seen), failed+limit)
	}
	// Per window: a need-sized first lookup, then full chunks and a remainder.
	if bound := (failed+limit+visibilityChunk-1)/visibilityChunk + 2*lake.reads; len(lake.lookups) > bound {
		t.Fatalf("%d visibility lookups over %d window reads, want <= %d", len(lake.lookups), lake.reads, bound)
	}
}

// historyLake serves AccountOperations end to end: the sourced arm and the
// participant arm honour cursor and LIMIT, and hydration returns the rows
// for exactly the keys it was sent.
type historyLake struct {
	participantLake
	sourced []accountOpKey // newest first
}

func (l *historyLake) Query(ctx context.Context, q string, args ...any) (driver.Rows, error) {
	switch {
	case isOpsBySourceProbe(q):
		return &stubRows{data: [][]any{{uint32(1)}}}, nil
	case strings.Contains(q, "FROM stellar.ops_by_source"):
		n := args[len(args)-1].(int)
		var out [][]any
		for _, k := range l.sourced {
			if len(args) == 6 && !(accountOpKey{args[2].(uint32), args[3].(uint32), args[4].(uint32)}).after(k) {
				continue
			}
			if len(out) == n {
				break
			}
			out = append(out, []any{k.ledger, k.txIndex, k.opIndex})
		}
		return &stubRows{data: out}, nil
	case strings.Contains(q, "FROM stellar.operations FINAL"):
		var out [][]any
		for _, t := range inTuples(q) {
			out = append(out, opRowFor(t[0], t[1], t[2]))
		}
		return &stubRows{data: out}, nil
	case isVisibilityLookup(q), strings.Contains(q, "stellar.operation_participants"):
		return l.participantLake.Query(ctx, q, args...)
	}
	return &stubRows{}, nil
}

// TestLatestLedgerAtOrBeforeQuery_CarriesLowerBound pins the lower bound that keeps the lake-tip lookup
// bounded: `ledger_seq <= X` alone prunes no partition. The window is LatestLedgerLookbackLedgers, matching
// the callers' clamps.
func TestLatestLedgerAtOrBeforeQuery_CarriesLowerBound(t *testing.T) {
	q := latestLedgerAtOrBeforeQuery
	if !strings.Contains(q, "ledger_seq BETWEEN ? AND ?") {
		t.Errorf("lake-tip lookup has no lower bound on ledger_seq — an at-or-before predicate scans every partition below the cursor:\n%s", q)
	}
	if strings.Contains(q, "ledger_seq <= ?") {
		t.Errorf("lake-tip lookup is back to an unbounded at-or-before predicate:\n%s", q)
	}
	// FINAL is load-bearing here and not a scan-cost item: it collapses the
	// un-merged ReplacingMergeTree duplicates that concentrate at the tip
	// this window reads, and costs nothing measurable once bounded.
	if !strings.Contains(q, "FROM stellar.ledgers FINAL") {
		t.Errorf("lake-tip lookup lost FINAL — an un-merged duplicate part decides the snapshot's ObservedAt:\n%s", q)
	}
	if !strings.Contains(q, "ORDER BY ledger_seq DESC LIMIT 1") {
		t.Errorf("lake-tip lookup must still return the NEWEST landed row in the window:\n%s", q)
	}
}

// TestLatestLedgerLookbackFloor pins the scan bound to the clamp constant, so widening one without the other
// fails. It also pins the underflow: maxSeq below the lookback reads from 0 instead of wrapping uint32.
func TestLatestLedgerLookbackFloor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		maxSeq uint32
		want   uint32
	}{
		{"tip", 64_277_149, 64_277_149 - LatestLedgerLookbackLedgers},
		{"one past the bound", LatestLedgerLookbackLedgers + 1, 1},
		{"exactly the bound", LatestLedgerLookbackLedgers, 0},
		{"inside the bound", LatestLedgerLookbackLedgers - 1, 0},
		{"genesis", 1, 0},
		{"zero", 0, 0},
	} {
		if got := latestLedgerLookbackFloor(tc.maxSeq); got != tc.want {
			t.Errorf("%s: latestLedgerLookbackFloor(%d) = %d, want %d", tc.name, tc.maxSeq, got, tc.want)
		}
	}
	if LatestLedgerLookbackLedgers != 512 {
		t.Errorf("LatestLedgerLookbackLedgers = %d, want 512 — it is also both callers' stalled-lake refusal bound (~45 min of chain); changing it changes when a supply snapshot refuses to publish", LatestLedgerLookbackLedgers)
	}
}

// Regression tests for ReplacingMergeTree dedup: reads using neither FINAL nor a dedup over-count unmerged
// rows. The stub harness has no real merge semantics, so these assert query shape (the dedup construct), not
// behaviour.

// opLightRowFor builds the 7-column opColsLight row scanOpsLight expects.
func opLightRowFor(seq uint32, txIndex, opIndex uint32) []any {
	return []any{seq, time.Unix(1700000000, 0).UTC(), "hash" + string(rune('0'+opIndex)), txIndex, opIndex, "OperationTypePayment", "GSOURCE"}
}

// opRowFor builds the 8-column opCols row scanOps expects (opColsLight +
// body_xdr).
func opRowFor(seq uint32, txIndex, opIndex uint32) []any {
	return []any{seq, time.Unix(1700000000, 0).UTC(), "hash" + string(rune('0'+opIndex)), txIndex, opIndex, "OperationTypePayment", "GSOURCE", "AAAAAA=="}
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
	// ledger+tx_hash-scoped so FINAL stays cheap (partition +
	// primary-key-prefix bounded), matching the sibling OperationsByLedger.
	if !strings.Contains(q, "FROM stellar.operations FINAL") {
		t.Fatalf("query = %q, want `FROM stellar.operations FINAL`", q)
	}
}

// Query-shape regressions for two ReplacingMergeTree over-count and mis-cap defects. The stubConn harness
// cannot model merge semantics, so these assert the emitted SQL carries the dedup or distinct-cap construct;
// live proof is in test/integration.

// TestContractActivitySummaryFor_DedupsRMTCount: the total and per-day counts must use DISTINCT ledger_seq
// (uniqExact), since a bare count() over the ReplacingMergeTree inflates with unmerged duplicate parts.
func TestContractActivitySummaryFor_DedupsRMTCount(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "contract_active_ledgers LIMIT 1"): // probe
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case strings.Contains(q, "FROM stellar.ledgers"): // settled split
			return &stubRows{data: [][]any{{uint32(100000)}}}, nil
		case strings.Contains(q, "min(close_time)") && strings.HasSuffix(q, ">= ?"): // live tail
			return &stubRows{data: [][]any{{time.Unix(1700050000, 0).UTC(), time.Unix(1700100000, 0).UTC(), uint64(2)}}}, nil
		case strings.Contains(q, "min(close_time)"): // memoised base
			if strings.Contains(q, "count()") {
				t.Fatalf("total query uses bare count() over un-merged RMT parts: %s", q)
			}
			if !strings.Contains(q, "uniqExact(ledger_seq)") {
				t.Fatalf("total query = %q, want toUInt64(uniqExact(ledger_seq))", q)
			}
			return &stubRows{data: [][]any{{
				time.Unix(1700000000, 0).UTC(), time.Unix(1700040000, 0).UTC(), uint64(5),
			}}}, nil
		default: // daily series
			if !strings.Contains(q, "ledger_seq >= ?") || conn.args[len(conn.args)-1][1] != uint32(1234) {
				t.Fatalf("daily query = %q args %v, want pruned on ledger_seq >= sinceLedger", q, conn.args[len(conn.args)-1])
			}
			if strings.Contains(q, "count()") {
				t.Fatalf("daily series uses bare count() over un-merged RMT parts: %s", q)
			}
			if !strings.Contains(q, "uniqExact(ledger_seq)") {
				t.Fatalf("daily query = %q, want toUInt64(uniqExact(ledger_seq)) per day", q)
			}
			return &stubRows{data: [][]any{
				{time.Unix(1700000000, 0).UTC(), uint64(3)},
			}}, nil
		}
	}
	r := &ExplorerReader{conn: conn}

	s, ok, err := r.ContractActivitySummaryFor(context.Background(), "CTESTCONTRACT", 30, 1234)
	if err != nil || !ok {
		t.Fatalf("ContractActivitySummaryFor: ok=%v err=%v", ok, err)
	}
	if s.ActiveLedgersTotal != 7 {
		t.Fatalf("ActiveLedgersTotal = %d, want 7", s.ActiveLedgersTotal)
	}
	if len(s.Daily) != 1 || s.Daily[0].ActiveLedgers != 3 {
		t.Fatalf("daily = %+v, want one day with 3 active ledgers", s.Daily)
	}
}
