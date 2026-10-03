package clickhouse

import (
	"context"
	"slices"
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

// wmFakeConn answers the watermark query (four columns) and the lake-min
// query (one column), counting every QueryRow on this one connection.
type wmFakeConn struct {
	driver.Conn
	queryRowCalls int
	lastArgs      []any
	wmVals        []uint64
}

func (c *wmFakeConn) QueryRow(_ context.Context, q string, args ...any) driver.Row {
	c.queryRowCalls++
	c.lastArgs = args
	if q == `SELECT toUInt64(min(ledger_seq)) FROM stellar.ledgers` {
		return &wmFakeRow{vals: []uint64{2}}
	}
	if c.wmVals != nil {
		return &wmFakeRow{vals: c.wmVals}
	}
	return &wmFakeRow{vals: []uint64{200, 0, 100, 200}} // chMax, firstGap, minPresent, maxInWindow
}

// TestWatermarkReader_ReusesConnectionAcrossCalls: every read a
// WatermarkReader serves runs on the one connection it holds.
func TestWatermarkReader_ReusesConnectionAcrossCalls(t *testing.T) {
	conn := &wmFakeConn{}
	w := &WatermarkReader{conn: conn}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		got, err := w.ContiguousWatermark(ctx, 100, 1_000)
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

// TestWatermarkReader_BoundsScanAndClamps: the gap scan is bound to [from, to]
// and the answer never exceeds to, even when the lake extends past it.
func TestWatermarkReader_BoundsScanAndClamps(t *testing.T) {
	conn := &wmFakeConn{wmVals: []uint64{200, 0, 100, 150}}
	w := &WatermarkReader{conn: conn}

	got, err := w.ContiguousWatermark(context.Background(), 100, 150)
	if err != nil {
		t.Fatalf("ContiguousWatermark: %v", err)
	}
	if got != 150 {
		t.Fatalf("ContiguousWatermark = %d, want 150 (clamped to the bound; lake tip is 200)", got)
	}
	if want := []any{uint32(100), uint32(150), uint32(100), uint32(100), uint32(150)}; !slices.Equal(conn.lastArgs, want) {
		t.Fatalf("query args = %v, want %v", conn.lastArgs, want)
	}
}

// TestWatermarkReader_HoleStraddlingBound: a hole running from inside the
// window past `to` is invisible to the bounded gap scan; the watermark must stop
// below it rather than report the whole window contiguous.
func TestWatermarkReader_HoleStraddlingBound(t *testing.T) {
	tests := []struct {
		name string
		vals []uint64 // chMax, firstGap, minPresent, maxInWindow
		from uint32
		to   uint32
		want uint32
	}{
		{"lake 100..200 missing 146..160, window [100,150]", []uint64{200, 0, 100, 145}, 100, 150, 145},
		{"single ledger missing at to", []uint64{200, 0, 100, 149}, 100, 150, 149},
		{"window wholly beyond the lake tip", []uint64{200, 0, 0, 0}, 300, 400, 299},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &WatermarkReader{conn: &wmFakeConn{wmVals: tt.vals}}
			got, err := w.ContiguousWatermark(context.Background(), tt.from, tt.to)
			if err != nil {
				t.Fatalf("ContiguousWatermark: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ContiguousWatermark = %d, want %d", got, tt.want)
			}
		})
	}
}
