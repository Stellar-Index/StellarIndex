package clickhouse

import (
	"context"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// wmFakeRow scans back fixed uint64 values, one per destination.
type wmFakeRow struct {
	driver.Row
	vals []uint64
}

func (r *wmFakeRow) Scan(dest ...any) error {
	for i := range dest {
		*dest[i].(*uint64) = r.vals[i]
	}
	return nil
}

// wmFakeConn answers the watermark query (three columns) and the lake-min
// query (one column), counting every QueryRow on this one connection.
type wmFakeConn struct {
	driver.Conn
	queryRowCalls int
}

func (c *wmFakeConn) QueryRow(_ context.Context, q string, _ ...any) driver.Row {
	c.queryRowCalls++
	if q == `SELECT toUInt64(min(ledger_seq)) FROM stellar.ledgers` {
		return &wmFakeRow{vals: []uint64{2}}
	}
	return &wmFakeRow{vals: []uint64{200, 0, 100}} // chMax, firstGap, minPresent
}

// TestWatermarkReader_ReusesConnectionAcrossCalls: every read a
// WatermarkReader serves runs on the one connection it holds.
func TestWatermarkReader_ReusesConnectionAcrossCalls(t *testing.T) {
	conn := &wmFakeConn{}
	w := &WatermarkReader{conn: conn}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		got, err := w.ContiguousWatermark(ctx, 100)
		if err != nil {
			t.Fatalf("call %d: ContiguousWatermark: %v", i, err)
		}
		if got != 200 {
			t.Fatalf("call %d: ContiguousWatermark = %d, want 200", i, got)
		}
	}
	lo, err := w.LakeMinLedger(ctx)
	if err != nil {
		t.Fatalf("LakeMinLedger: %v", err)
	}
	if lo != 2 {
		t.Fatalf("LakeMinLedger = %d, want 2", lo)
	}
	if conn.queryRowCalls != 4 {
		t.Fatalf("queryRowCalls = %d, want 4 (every read on the reader's one connection)", conn.queryRowCalls)
	}
}
