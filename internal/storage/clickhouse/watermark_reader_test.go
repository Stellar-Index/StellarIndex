package clickhouse

import (
	"context"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// wmFakeRow scans back a fixed (chMax, firstGap, minPresent) triple —
// the exact shape contiguousWatermarkOn's query scans into.
type wmFakeRow struct {
	driver.Row
	chMax, firstGap, minPresent uint64
}

func (r *wmFakeRow) Scan(dest ...any) error {
	*dest[0].(*uint64) = r.chMax
	*dest[1].(*uint64) = r.firstGap
	*dest[2].(*uint64) = r.minPresent
	return nil
}

// wmFakeConn counts QueryRow calls so a test can prove a WatermarkReader
// issues one query per call on ONE connection, never re-dialing.
type wmFakeConn struct {
	driver.Conn
	queryRowCalls int
}

func (c *wmFakeConn) QueryRow(context.Context, string, ...any) driver.Row {
	c.queryRowCalls++
	return &wmFakeRow{chMax: 200, firstGap: 0, minPresent: 100}
}

// TestWatermarkReader_ReusesConnectionAcrossCalls pins T360: ContiguousWatermark
// dials a fresh connection (openRead) and tears it down on every single call —
// wasteful for a caller that polls it on a cadence (the real-time projector,
// every cycle). WatermarkReader must issue one query per call on the SAME
// connection it opened once, not re-dial.
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
	if conn.queryRowCalls != 3 {
		t.Fatalf("queryRowCalls = %d, want 3 (one WatermarkReader, one query per call, no re-dial)",
			conn.queryRowCalls)
	}
}
