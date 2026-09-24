package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

func isOpsBySourceProbe(q string) bool {
	return strings.Contains(q, "stellar.ops_by_source") && !strings.Contains(q, "WHERE")
}

// withOpsBySourceRows answers the ops_by_source availability probe with one
// row (a provisioned, populated projection) and hands every other query to
// next. Tests of the account-history SQL shape use it so they exercise the
// history queries rather than the refusal.
func withOpsBySourceRows(next func(string) (driver.Rows, error)) func(string) (driver.Rows, error) {
	return func(q string) (driver.Rows, error) {
		if isOpsBySourceProbe(q) {
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		}
		return next(q)
	}
}

// opsBySourceLake is one ClickHouse whose stellar.ops_by_source exists and
// either holds rows or is empty (DDL applied but not yet fed/backfilled, or
// TRUNCATEd under the running reader). Every history read answers empty.
type opsBySourceLake struct{ rows bool }

func (l *opsBySourceLake) respond(q string) (driver.Rows, error) {
	if isOpsBySourceProbe(q) && l.rows {
		return &stubRows{data: [][]any{{uint32(1)}}}, nil
	}
	return &stubRows{}, nil
}

// accountHistoryErrs runs the three ops_by_source-gated readers once each.
func accountHistoryErrs(ctx context.Context, r *ExplorerReader) map[string]error {
	const account = "GTESTOPSBYSOURCE"
	_, txErr := r.AccountTransactions(ctx, account, 10, ExplorerCursor{})
	_, opErr := r.AccountOperations(ctx, account, 10, ExplorerCursor{})
	_, cntErr := r.AccountOperationTypeCounts(ctx, account)
	return map[string]error{
		"AccountTransactions":        txErr,
		"AccountOperations":          opErr,
		"AccountOperationTypeCounts": cntErr,
	}
}

// TestAccountHistory_RefusesEmptyOpsBySource pins T394: an existing-but-EMPTY
// stellar.ops_by_source must not be treated as provisioned. The readers
// serve an empty sourced arm as "this account sourced nothing", so an unfed
// projection would answer every account's own history as empty (or
// participant-only) with no error, instead of the fail-loud refusal.
func TestAccountHistory_RefusesEmptyOpsBySource(t *testing.T) {
	lake := &opsBySourceLake{rows: false}
	conn := &stubConn{respond: lake.respond}
	r := &ExplorerReader{conn: conn}

	for name, err := range accountHistoryErrs(context.Background(), r) {
		if !errors.Is(err, errOpsBySourceMissing) {
			t.Errorf("%s over an empty ops_by_source: err = %v, want errOpsBySourceMissing", name, err)
		}
	}
	if n := len(conn.queries) - countQueries(conn.queries, isOpsBySourceProbe); n != 0 {
		t.Errorf("%d history queries ran over an empty ops_by_source, want 0: %q", n, conn.queries)
	}
}

// TestAccountHistory_OpsBySourceTruncatedUnderReader: a populated projection
// serves; once it is emptied under the running process the next caller past
// the lease refuses; once it is repopulated the refusal lifts without a
// restart (an empty verdict is never latched).
func TestAccountHistory_OpsBySourceTruncatedUnderReader(t *testing.T) {
	lake := &opsBySourceLake{rows: true}
	conn := &stubConn{respond: lake.respond}
	clk := newFakeClock()
	r := &ExplorerReader{conn: conn, opsBySourceProbe: schemaProbe{now: clk.now}}
	ctx := context.Background()

	for name, err := range accountHistoryErrs(ctx, r) {
		if err != nil {
			t.Fatalf("populated: %s: %v", name, err)
		}
	}

	lake.rows = false
	clk.advance(schemaProbeLease + time.Second)
	for name, err := range accountHistoryErrs(ctx, r) {
		if !errors.Is(err, errOpsBySourceMissing) {
			t.Errorf("truncated: %s: err = %v, want errOpsBySourceMissing", name, err)
		}
	}

	lake.rows = true
	clk.advance(schemaProbeRetryAfter + time.Second)
	for name, err := range accountHistoryErrs(ctx, r) {
		if err != nil {
			t.Errorf("repopulated: %s: %v", name, err)
		}
	}
}
