package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Tests for the per-account activity watermark bound: AccountOperations'
// key arms read `ORDER BY pk DESC LIMIT n`, which streams granules backwards
// from the TIP until the account's rows turn up (~4 s for a 46d-idle account). With a watermark
// row in stellar.account_activity the reader bounds EACH arm's resolve with
// `ledger_seq <= watermark`, so the reverse read starts at the account's real
// last activity. These pin the emitted SQL and the bind order (stubConn from
// tx_hash_index_test.go — SQL shape assertions, same convention as
// explorer_reader_union_bounds_test.go); the live-ClickHouse correctness proof
// — bounded rows == unbounded rows, incl. a participant row ABOVE the sourced
// watermark — is test/integration/clickhouse_accounts_test.go.

// Query-shape classifiers for the watermark reads.
func isAccountActivityProbe(q string) bool {
	return strings.Contains(q, "stellar.account_activity") && !strings.Contains(q, "WHERE")
}

func isAccountActivityLookup(q string) bool {
	return strings.Contains(q, "max(last_ledger)") &&
		strings.Contains(q, "stellar.account_activity") &&
		strings.Contains(q, "WHERE account_id = ?")
}

// watermarkStubConn routes the ops_by_source probe and the account_activity
// probe + lookup to real single-column rows (so the reader takes the bounded
// path) and everything else to `rows`.
func watermarkStubConn(watermark uint32, rows *stubRows) *stubConn {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isOpsBySourceProbe(q):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case isAccountActivityProbe(q):
			return &stubRows{data: [][]any{{watermark}}}, nil
		case isAccountActivityLookup(q):
			return &stubRows{data: [][]any{{watermark}}}, nil
		default:
			return rows, nil
		}
	}
	return conn
}

// keyReads returns the emitted key reads (ops_by_source / participants) and
// their args.
func keyReads(conn *stubConn) (qs []string, args [][]any) {
	for i, q := range conn.queries {
		if isOpsBySourceProbe(q) || isVisibilityLookup(q) {
			continue
		}
		if strings.Contains(q, "FROM stellar.ops_by_source") || strings.Contains(q, "FROM stellar.operation_participants") {
			qs, args = append(qs, q), append(args, conn.args[i])
		}
	}
	return qs, args
}

func TestAccountOperations_WatermarkBoundsEachArmResolve(t *testing.T) {
	const (
		limit     = 37
		watermark = uint32(63_411_270)
	)
	conn := watermarkStubConn(watermark, &stubRows{})
	if _, _, err := (&ExplorerReader{conn: conn}).AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	// BOTH arms must carry the bound — a bound on one arm, or on the
	// hydration only, leaves the other arm's reverse read walking the tip.
	qs, args := keyReads(conn)
	if len(qs) != 2 {
		t.Fatalf("key reads = %d, want 2 (sourced + participant windows): %v", len(qs), conn.queries)
	}
	for i, q := range qs {
		if !strings.Contains(q, "AND ledger_seq <= ?") || args[i][1] != watermark {
			t.Errorf("key read %d is unbounded or binds %v as the bound (#31):\n%s", i, args[i], q)
		}
	}
	q, got := exactOpsArm(t, watermarkStubConn(watermark, &stubRows{}).respond, limit, ExplorerCursor{})
	assertArgs(t, q, got, []any{"GTEST", watermark, limit})
}

func TestAccountOperations_WatermarkPreservesCursorArgOrder(t *testing.T) {
	const (
		limit     = 9
		watermark = uint32(63_411_270)
	)
	cur := ExplorerCursor{Ledger: 63_000_000, A: 4, B: 2}
	q, got := exactOpsArm(t, watermarkStubConn(watermark, &stubRows{}).respond, limit, cur)
	// The bound precedes the cursor tuple in the text, so it must in the args.
	if i, j := strings.Index(q, "ledger_seq <= ?"), strings.Index(q, "< (?, ?, ?)"); !(i >= 0 && j > i) {
		t.Fatalf("bound clause must precede the cursor clause in the emitted SQL:\n%s", q)
	}
	assertArgs(t, q, got, []any{"GTEST", watermark, cur.Ledger, cur.Ledger, cur.A, cur.B, limit})
}

// TestAccountOperations_NoWatermarkFallsBackUnbounded pins the degrade
// direction: no watermark row (max() over zero rows scans as 0) → no bound on
// any key read. A missing watermark may only cost performance; a fabricated
// bound of 0 would hide the account's whole history.
func TestAccountOperations_NoWatermarkFallsBackUnbounded(t *testing.T) {
	const limit = 37
	conn := watermarkStubConn(0, &stubRows{})
	if _, _, err := (&ExplorerReader{conn: conn}).AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	qs, _ := keyReads(conn)
	for _, q := range qs {
		if strings.Contains(q, "ledger_seq <= ?") {
			t.Fatalf("no watermark, yet a key read carries a bound:\n%s", q)
		}
	}
	q, got := exactOpsArm(t, watermarkStubConn(0, &stubRows{}).respond, limit, ExplorerCursor{})
	assertArgs(t, q, got, []any{"GTEST", limit})
}

func assertArgs(t *testing.T, q string, got, want []any) {
	t.Helper()
	if n := strings.Count(q, "?"); n != len(got) {
		t.Fatalf("query has %d placeholders, reader bound %d args: %v\n%s", n, len(got), got, q)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

// stellar.operations is ReplacingMergeTree, so an account
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
	rows, _, err := (&ExplorerReader{conn: conn}).AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{})
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
