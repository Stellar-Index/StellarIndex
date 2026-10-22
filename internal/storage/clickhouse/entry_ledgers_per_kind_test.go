package clickhouse

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type entryLedgerConn struct {
	driver.Conn
	q    string
	args []any
	rows []uint32
}

func (c *entryLedgerConn) Query(_ context.Context, q string, args ...any) (driver.Rows, error) {
	c.q, c.args = q, args
	return &entryLedgerRows{ls: c.rows}, nil
}

type entryLedgerRows struct {
	driver.Rows
	ls []uint32
	i  int
}

func (r *entryLedgerRows) Next() bool { r.i++; return r.i <= len(r.ls) }
func (r *entryLedgerRows) Scan(dest ...any) error {
	*dest[0].(*uint32) = r.ls[r.i-1]
	return nil
}
func (r *entryLedgerRows) Err() error   { return nil }
func (r *entryLedgerRows) Close() error { return nil }

// Each kind must be its own key-prefix arm so ClickHouse reads it newest-first instead of sorting every row of the key.
func TestAssetEntryLedgers_OneNewestFirstArmPerKind(t *testing.T) {
	c := &entryLedgerConn{rows: []uint32{90, 80}}
	r := &ExplorerReader{conn: c}
	got, err := r.AssetEntryLedgers(context.Background(), "a", []string{"trustline", "offer"}, 100, 5)
	if err != nil || !reflect.DeepEqual(got, []uint32{90, 80}) {
		t.Fatalf("AssetEntryLedgers = %v, %v", got, err)
	}
	if n := strings.Count(c.q, "WHERE asset = ? AND entry_type = ? AND ledger <= ?"); n != 2 {
		t.Fatalf("query has %d per-kind arms, want 2: %s", n, c.q)
	}
	if strings.Contains(c.q, "IN (?)") || !strings.Contains(c.q, "SELECT DISTINCT ledger FROM (") {
		t.Fatalf("query shape: %s", c.q)
	}
	want := []any{"a", "trustline", uint32(100), 5, "a", "offer", uint32(100), 5, 5}
	if !reflect.DeepEqual(c.args, want) {
		t.Fatalf("args = %v, want %v", c.args, want)
	}
}

func TestAccountEntryLedgers_NoKindsSkipsRead(t *testing.T) {
	c := &entryLedgerConn{}
	r := &ExplorerReader{conn: c}
	got, err := r.AccountEntryLedgers(context.Background(), "k", nil, 100, 5)
	if err != nil || got != nil || c.q != "" {
		t.Fatalf("got %v, %v, query %q; want no read", got, err, c.q)
	}
}
