package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// usageDailyExec is one DML statement the fake driver received, with
// its bind arguments in order.
type usageDailyExec struct {
	query string
	args  []driver.Value
}

// usageDailyConn is the narrowest driver that lets UpsertUsageDaily run
// to completion either way it is written — as a transaction of
// per-row prepared execs or as autocommitted multi-row statements —
// and records what reached the wire. The statement count IS the
// round-trip count, which is the property under test.
type usageDailyConn struct {
	execs  []usageDailyExec
	begins int
}

func (c *usageDailyConn) Prepare(q string) (driver.Stmt, error) {
	return &usageDailyStmt{c: c, query: q}, nil
}
func (c *usageDailyConn) Close() error { return nil }

func (c *usageDailyConn) Begin() (driver.Tx, error) {
	c.begins++
	return usageDailyTx{}, nil
}

func (c *usageDailyConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.record(q, args)
	return driver.RowsAffected(1), nil
}

func (c *usageDailyConn) record(q string, args []driver.NamedValue) {
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	c.execs = append(c.execs, usageDailyExec{query: q, args: vals})
}

type usageDailyStmt struct {
	c     *usageDailyConn
	query string
}

func (s *usageDailyStmt) Close() error  { return nil }
func (s *usageDailyStmt) NumInput() int { return -1 }
func (s *usageDailyStmt) Exec(args []driver.Value) (driver.Result, error) {
	named := make([]driver.NamedValue, len(args))
	for i, a := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	s.c.record(s.query, named)
	return driver.RowsAffected(1), nil
}

func (s *usageDailyStmt) Query([]driver.Value) (driver.Rows, error) {
	return nil, errors.New("usageDailyStmt: Query not implemented")
}

type usageDailyTx struct{}

func (usageDailyTx) Commit() error   { return nil }
func (usageDailyTx) Rollback() error { return nil }

type usageDailyConnector struct{ conn *usageDailyConn }

func (c usageDailyConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c usageDailyConnector) Driver() driver.Driver                        { return usageDailyDriver{} }

type usageDailyDriver struct{}

func (usageDailyDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("usageDailyDriver: Open not implemented")
}

// Pinned independently of the production constants so the test still
// compiles — and fails on the round-trip count — against a per-row
// implementation.
const (
	usageDailyTestChunk   = 500
	usageDailyTestColumns = 7
)

func newUsageDailyStore() (*Store, *usageDailyConn) {
	conn := &usageDailyConn{}
	db := sql.OpenDB(usageDailyConnector{conn: conn})
	db.SetMaxOpenConns(1)
	return &Store{db: db}, conn
}

func usageDailyRows(n int) []usage.RollupRow {
	rows := make([]usage.RollupRow, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, usage.RollupRow{
			Day:      "2026-09-19",
			Subject:  fmt.Sprintf("id:acct:%d", i),
			Endpoint: "/v1/price",
			OK:       int64(i + 1),
		})
	}
	return rows
}

// TestUpsertUsageDaily_OneStatementPerBatch — a batch of rows is one
// multi-row upsert, not one round-trip per row inside a transaction.
func TestUpsertUsageDaily_OneStatementPerBatch(t *testing.T) {
	store, conn := newUsageDailyStore()
	if err := store.UpsertUsageDaily(context.Background(), usageDailyRows(3)); err != nil {
		t.Fatal(err)
	}
	if len(conn.execs) != 1 {
		t.Fatalf("statements = %d for 3 rows, want 1 multi-row upsert (one round-trip per row is the defect)", len(conn.execs))
	}
	got := conn.execs[0]
	if tuples := strings.Count(got.query, "($"); tuples != 3 {
		t.Errorf("VALUES tuples = %d, want 3 in:\n%s", tuples, got.query)
	}
	if len(got.args) != 3*usageDailyTestColumns {
		t.Errorf("bind args = %d, want %d", len(got.args), 3*usageDailyTestColumns)
	}
	if !strings.Contains(got.query, "$21)") {
		t.Errorf("last placeholder should be $21, query:\n%s", got.query)
	}
	for _, want := range []string{
		"ON CONFLICT (day, subject, endpoint) DO UPDATE",
		"GREATEST(usage_daily.ok_count,           EXCLUDED.ok_count)",
		"GREATEST(usage_daily.throttled_count,    EXCLUDED.throttled_count)",
	} {
		if !strings.Contains(got.query, want) {
			t.Errorf("query lost %q:\n%s", want, got.query)
		}
	}
	if conn.begins != 0 {
		t.Errorf("explicit transactions = %d, want 0 (each chunk autocommits)", conn.begins)
	}
}

// TestUpsertUsageDaily_ChunksLargeBatch — a batch larger than one
// chunk is split into bounded statements, never one unbounded one and
// never one per row.
func TestUpsertUsageDaily_ChunksLargeBatch(t *testing.T) {
	store, conn := newUsageDailyStore()
	n := 2*usageDailyTestChunk + 201
	if err := store.UpsertUsageDaily(context.Background(), usageDailyRows(n)); err != nil {
		t.Fatal(err)
	}
	wantArgs := []int{
		usageDailyTestChunk * usageDailyTestColumns,
		usageDailyTestChunk * usageDailyTestColumns,
		201 * usageDailyTestColumns,
	}
	if len(conn.execs) != len(wantArgs) {
		t.Fatalf("statements = %d for %d rows, want %d chunks of <= %d rows",
			len(conn.execs), n, len(wantArgs), usageDailyTestChunk)
	}
	for i, want := range wantArgs {
		if got := len(conn.execs[i].args); got != want {
			t.Errorf("chunk %d bind args = %d, want %d", i, got, want)
		}
	}
	// The last row of the batch must be the last tuple of the last chunk.
	last := conn.execs[2].args
	if last[len(last)-usageDailyTestColumns+1] != fmt.Sprintf("id:acct:%d", n-1) {
		t.Errorf("last chunk ends on %v, want id:acct:%d", last[len(last)-usageDailyTestColumns+1], n-1)
	}
}

// TestUpsertUsageDaily_FoldsDuplicateKeys — two rows for one key in a
// batch become a single tuple carrying the per-column maximum, exactly
// what the GREATEST merge would have reached applying them in turn
// (Postgres rejects the same conflict target twice in one statement).
func TestUpsertUsageDaily_FoldsDuplicateKeys(t *testing.T) {
	store, conn := newUsageDailyStore()
	rows := []usage.RollupRow{
		{Day: "2026-09-19", Subject: "id:acct:1", Endpoint: "/v1/price", OK: 5, ClientErrors: 9, Throttled: 1},
		{Day: "2026-09-19", Subject: "id:acct:1", Endpoint: "/v1/price", OK: 9, ClientErrors: 2, ServerErrors: 4},
	}
	if err := store.UpsertUsageDaily(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	if len(conn.execs) != 1 || len(conn.execs[0].args) != usageDailyTestColumns {
		t.Fatalf("execs = %+v, want one statement of one tuple", conn.execs)
	}
	got := conn.execs[0].args
	want := []driver.Value{"2026-09-19", "id:acct:1", "/v1/price", int64(9), int64(9), int64(4), int64(1)}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestUpsertUsageDaily_IncompleteRowWritesNothing — validation covers
// the whole batch before the first statement; a bad row anywhere means
// nothing from that batch reaches Postgres.
func TestUpsertUsageDaily_IncompleteRowWritesNothing(t *testing.T) {
	store, conn := newUsageDailyStore()
	rows := append(usageDailyRows(2), usage.RollupRow{Day: "2026-09-19", Subject: "id:acct:9"})
	err := store.UpsertUsageDaily(context.Background(), rows)
	if err == nil || !strings.Contains(err.Error(), "incomplete row") {
		t.Fatalf("err = %v, want incomplete-row error", err)
	}
	if len(conn.execs) != 0 {
		t.Errorf("statements = %d after a rejected batch, want 0", len(conn.execs))
	}
}
