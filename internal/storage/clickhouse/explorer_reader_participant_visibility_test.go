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
// cursor and LIMIT, so participantKeys' paging runs for real.
type participantLake struct {
	driver.Conn
	rows    []accountOpKey // newest first; a key may repeat (un-merged parts)
	failed  map[accountTxKey]bool
	lookups []string
}

func (l *participantLake) Query(_ context.Context, q string, args ...any) (driver.Rows, error) {
	if isVisibilityLookup(q) {
		l.lookups = append(l.lookups, q)
		if len(args) != 1 || args[0] != "GTEST" {
			return nil, fmt.Errorf("lookup must bind only the account, got %v", args)
		}
		var out [][]any
		for _, k := range l.rows {
			if !l.failed[accountTxKey{k.ledger, k.txIndex}] {
				out = append(out, []any{k.ledger, k.txIndex})
			}
		}
		return &stubRows{data: out}, nil
	}
	if !strings.Contains(q, "FROM stellar.operation_participants WHERE account = ?") || strings.Contains(q, "LIMIT 1 BY") {
		return nil, fmt.Errorf("unexpected query: %s", q)
	}
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
			var got []accountOpKey
			var from *accountOpKey
			for page := 0; ; page++ {
				keys, err := participantKeys(context.Background(), lake, "GTEST", limit, opParticipantArm, "", nil, from)
				if err != nil {
					t.Fatalf("page %d: %v", page, err)
				}
				got = append(got, keys...)
				if len(keys) < limit {
					break
				}
				from = &keys[len(keys)-1]
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
	if _, err := (&ExplorerReader{conn: conn}).AccountTransactions(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
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
	if _, err := (&ExplorerReader{conn: conn}).AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	last := conn.queries[len(conn.queries)-1]
	if !strings.Contains(last, "IN ((99,0,0),(98,0,0),(97,0,0))") {
		t.Fatalf("hydration must skip both ops of the failed participant tx and still fill the page: %s", last)
	}
}

// A page one slot short of full, followed by a long failed run, must not be
// probed key by key: lookups are chunked at a fixed size, independent of the
// limit and the free slots.
func TestParticipantKeys_FailedRunLookupsAreChunked(t *testing.T) {
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

	got, err := participantKeys(context.Background(), lake, "GTEST", limit, opParticipantArm, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("want the %d visible keys in order, got %d: %v", len(want), len(got), got)
	}
	total := failed + limit
	// Window doubling re-reads nothing; every key is looked up once, plus one
	// partial chunk per window (13 windows from 4*limit up to the cap).
	bound := (total+visibilityChunk-1)/visibilityChunk + 16
	t.Logf("%d visibility lookups for %d keys (bound %d)", len(lake.lookups), total, bound)
	if len(lake.lookups) > bound {
		t.Fatalf("%d visibility lookups for %d keys, want <= %d", len(lake.lookups), total, bound)
	}
}
