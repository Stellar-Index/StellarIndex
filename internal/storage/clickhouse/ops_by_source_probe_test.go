package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

func isOpsBySourceProbe(q string) bool {
	return (strings.Contains(q, "stellar.ops_by_source") && !strings.Contains(q, "WHERE")) || q == opsBySourceProofProbeQuery
}

func isOpTypesCoverageProbe(q string) bool {
	return strings.Contains(q, "stellar.account_op_types_coverage")
}

// withOpsBySourceRows answers the ops_by_source availability probe with one row (a provisioned, populated
// projection) and hands every other query to next, so account-history SQL-shape tests exercise the history
// queries, not the refusal.
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
	_, _, txErr := r.AccountTransactions(ctx, account, 10, ExplorerCursor{})
	_, _, opErr := r.AccountOperations(ctx, account, 10, ExplorerCursor{})
	_, cntErr := r.AccountOperationTypeCounts(ctx, account)
	return map[string]error{
		"AccountTransactions":        txErr,
		"AccountOperations":          opErr,
		"AccountOperationTypeCounts": cntErr,
	}
}

// TestAccountHistory_RefusesEmptyOpsBySource pins that an existing-but-EMPTY stellar.ops_by_source is not
// provisioned: readers treat an empty sourced arm as "sourced nothing", so an unfed projection would
// silently answer empty histories instead of refusing loudly.
func TestAccountHistory_RefusesEmptyOpsBySource(t *testing.T) {
	lake := &opsBySourceLake{rows: false}
	conn := &stubConn{respond: lake.respond}
	r := &ExplorerReader{conn: conn}

	for name, err := range accountHistoryErrs(context.Background(), r) {
		if !errors.Is(err, errOpsBySourceMissing) {
			t.Errorf("%s over an empty ops_by_source: err = %v, want errOpsBySourceMissing", name, err)
		}
	}
	isProbe := func(q string) bool { return isOpsBySourceProbe(q) || isOpTypesCoverageProbe(q) }
	if n := len(conn.queries) - countQueries(conn.queries, isProbe); n != 0 {
		t.Errorf("%d history queries ran over an empty ops_by_source, want 0: %q", n, conn.queries)
	}
}

// TestAccountHistory_OpsBySourceTruncatedUnderReader: a populated projection serves; once emptied under the
// running process the next caller past the lease refuses; once repopulated the refusal lifts without a
// restart.
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

// TestAccountOperationTypeCounts_MarkerSwitchesToIndexedTable: with the account_op_types coverage marker the counts read the keyed
// table and need no ops_by_source; without it they take the two-arm join.
func TestAccountOperationTypeCounts_MarkerSwitchesToIndexedTable(t *testing.T) {
	marker := true
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		if isOpTypesCoverageProbe(q) && marker {
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		}
		return (&opsBySourceLake{rows: true}).respond(q)
	}}
	clk := newFakeClock()
	r := &ExplorerReader{conn: conn, opTypesCoverageProbe: schemaProbe{now: clk.now}}
	ctx := context.Background()

	if _, err := r.AccountOperationTypeCounts(ctx, "GTESTOPTYPES"); err != nil {
		t.Fatalf("marked: %v", err)
	}
	if last := conn.queries[len(conn.queries)-1]; last != accountOpTypeCountsIndexedQuery {
		t.Fatalf("marked: ran %q, want accountOpTypeCountsIndexedQuery", last)
	}
	if n := countQueries(conn.queries, isOpsBySourceProbe); n != 0 {
		t.Errorf("marked: probed ops_by_source %d times, want 0", n)
	}

	marker = false
	clk.advance(schemaProbeLease + time.Second)
	if _, err := r.AccountOperationTypeCounts(ctx, "GTESTOPTYPES"); err != nil {
		t.Fatalf("unmarked: %v", err)
	}
	if last := conn.queries[len(conn.queries)-1]; last != accountOpTypeCountsQuery {
		t.Fatalf("unmarked: ran %q, want accountOpTypeCountsQuery", last)
	}
}

// TestOpsBySourceAvailable_UnprovenServesAndReportsAbsent: without ch-index-proof's row the projection still serves
// (there is no fallback to refuse to), and the proof probe answers absent so the schema-probe alert pages.
func TestOpsBySourceAvailable_UnprovenServesAndReportsAbsent(t *testing.T) {
	for _, proven := range []bool{false, true} {
		conn := &stubConn{respond: func(q string) (driver.Rows, error) {
			if q == opsBySourceProofProbeQuery {
				if proven {
					return &stubRows{data: [][]any{{uint32(65_000_000)}}}, nil
				}
				return &stubRows{}, nil
			}
			if isOpsBySourceProbe(q) {
				return &stubRows{data: [][]any{{uint32(1)}}}, nil
			}
			return &stubRows{}, nil
		}}
		r := &ExplorerReader{conn: conn, opsBySourceProofProbe: schemaProbe{name: "ops_by_source_proof"}}
		if !r.opsBySourceAvailable(context.Background()) {
			t.Fatalf("proven=%v: a populated ops_by_source must serve", proven)
		}
		want := 0.0
		if proven {
			want = 1
		}
		if got := testutil.ToFloat64(obs.CHSchemaProbePresent.WithLabelValues("ops_by_source_proof")); got != want {
			t.Errorf("proven=%v: ops_by_source_proof probe present = %v, want %v", proven, got, want)
		}
	}
}
