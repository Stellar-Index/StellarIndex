package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// participantLake is one account's stellar.operation_participants rows plus
// the failed transactions among them. It honours the participant reads'
// cursor and LIMIT and answers a visibility lookup only for the keys it was
// sent, so participantKeys' paging and chunking run for real.
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

// TestParticipantKeys_ExactPastFailedTxs walks the participant arm page by
// page and requires exactly the visible keys, in order, with every page but
// the last full: a failed tx must neither appear nor shorten a page (a short
// page reads as end of history, #290), even when a whole window is failed.
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

// A run of planted failed txs too long for one request's budget ends the
// page short at a scan frontier; paging on from the frontier visits every
// key exactly once and returns exactly the visible ones. Lookups stay chunked
// at a fixed size, independent of the limit and the free slots.
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

// The reader-level arm: a failed participant tx is skipped and the next
// visible one fills its slot; the account's sourced keys are unaffected.
func TestAccountTransactions_SkipsFailedParticipantTxs(t *testing.T) {
	const limit = 3
	router := &txArmRouter{
		sourced:     [][]any{txKeyRow(98, 0)},
		participant: [][]any{txKeyRow(100, 0), txKeyRow(99, 0), txKeyRow(97, 0)},
		failed:      map[[2]uint32]bool{{100, 0}: true},
	}
	conn := &stubConn{respond: withOpsBySourceRows(router.respond)}
	if _, _, err := (&ExplorerReader{conn: conn}).AccountTransactions(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountTransactions: %v", err)
	}
	last := conn.queries[len(conn.queries)-1]
	if !strings.Contains(last, "IN ((99,0),(98,0),(97,0))") {
		t.Fatalf("hydration must skip the failed participant tx and still fill the page: %s", last)
	}
}

func TestAccountOperations_SkipsFailedParticipantTxs(t *testing.T) {
	const limit = 3
	router := &opsArmRouter{
		sourced:     [][]any{keyRow(98, 0, 0)},
		participant: [][]any{keyRow(100, 0, 1), keyRow(100, 0, 0), keyRow(99, 0, 0), keyRow(97, 0, 0)},
		failed:      map[[2]uint32]bool{{100, 0}: true},
	}
	conn := &stubConn{respond: withOpsBySourceRows(router.respond)}
	if _, _, err := (&ExplorerReader{conn: conn}).AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	last := conn.queries[len(conn.queries)-1]
	if !strings.Contains(last, "IN ((99,0,0),(98,0,0),(97,0,0))") {
		t.Fatalf("hydration must skip both ops of the failed participant tx and still fill the page: %s", last)
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

// When the participant arm stops at its frontier, the reader serves a short
// page with resume at that frontier, drops sourced keys past it, and the
// next page carries on with no row skipped or repeated.
func TestAccountOperations_BudgetResumeIsExact(t *testing.T) {
	const (
		limit  = 50
		failed = 10000
	)
	lake := &historyLake{participantLake: participantLake{failed: map[accountTxKey]bool{}}}
	var want []accountOpKey
	ledger := uint32(failed + 2*limit)
	for i := 0; i < limit/2; i++ {
		k := accountOpKey{ledger, 1, 0}
		lake.rows = append(lake.rows, k)
		want = append(want, k)
		ledger--
	}
	for i := 0; i < failed; i++ {
		lake.rows = append(lake.rows, accountOpKey{ledger, 1, 0})
		lake.failed[accountTxKey{ledger, 1}] = true
		if i%200 == 0 { // the account's own ops, inside and past the run
			k := accountOpKey{ledger, 0, 0}
			lake.sourced = append(lake.sourced, k)
			want = append(want, k)
		}
		ledger--
	}
	tail := accountOpKey{ledger, 1, 0}
	lake.rows = append(lake.rows, tail)
	want = append(want, tail)

	r := &ExplorerReader{conn: lake}
	var got []accountOpKey
	var cur ExplorerCursor
	resumes := 0
	for page := 0; page < 1000; page++ {
		rows, resume, err := r.AccountOperations(context.Background(), "GTEST", limit, cur)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, o := range rows {
			got = append(got, accountOpKey{o.Seq, o.TxIndex, o.OpIndex})
		}
		switch {
		case resume.IsSet():
			if len(rows) >= limit {
				t.Fatalf("page %d: resume on a full page", page)
			}
			resumes++
			cur = resume
		case len(rows) == limit:
			last := rows[len(rows)-1]
			cur = ExplorerCursor{Ledger: last.Seq, A: last.TxIndex, B: last.OpIndex}
		default:
			if resumes == 0 {
				t.Fatal("the walk never stopped at a frontier")
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("walk returned %d rows, want %d in order\n got: %v\nwant: %v", len(got), len(want), got, want)
			}
			return
		}
	}
	t.Fatal("walk did not end")
}
