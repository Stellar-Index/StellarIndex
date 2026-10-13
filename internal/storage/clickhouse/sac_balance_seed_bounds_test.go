package clickhouse

import (
	"context"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type boundsFakeConn struct {
	driver.Conn
	queries []string
}

func (c *boundsFakeConn) QueryRow(_ context.Context, query string, _ ...any) driver.Row {
	c.queries = append(c.queries, query)
	return boundsFakeRow{lo: 3, hi: 64927298}
}

type boundsFakeRow struct{ lo, hi uint32 }

func (r boundsFakeRow) Scan(dest ...any) error {
	*(dest[0].(*uint32)) = r.lo
	*(dest[1].(*uint32)) = r.hi
	return nil
}

func (boundsFakeRow) ScanStruct(any) error { return nil }
func (boundsFakeRow) Err() error           { return nil }

// The bounds must come from a key-ordered LIMIT 1 read at each end of the table, never from an aggregate over every part.
func TestEntryChangeLedgerBounds_ReadsEachEndInKeyOrder(t *testing.T) {
	conn := &boundsFakeConn{}
	lo, hi, err := entryChangeLedgerBounds(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if lo != 3 || hi != 64927298 {
		t.Fatalf("bounds = [%d, %d], want [3, 64927298]", lo, hi)
	}
	if len(conn.queries) != 1 {
		t.Fatalf("queries = %d, want one round trip", len(conn.queries))
	}
	q := conn.queries[0]
	if strings.Count(q, "FROM stellar.ledger_entry_changes ORDER BY ledger_seq") != 2 {
		t.Errorf("every read of ledger_entry_changes must be in primary-key order:\n%s", q)
	}
	if strings.Count(q, "LIMIT 1") != 2 {
		t.Errorf("each end must be a LIMIT 1 read:\n%s", q)
	}
}
