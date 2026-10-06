package clickhouse

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

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
