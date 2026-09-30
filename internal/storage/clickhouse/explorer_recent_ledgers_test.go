package clickhouse

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ledgerLakeStub serves stellar.ledgers reads over an in-memory, possibly
// holed set of ledger sequences, honouring each RecentLedgers query shape's
// bound args.
func ledgerLakeStub(t *testing.T, present func(uint32) bool, tip uint32) *stubConn {
	t.Helper()
	c := &stubConn{}
	c.respond = func(q string) (driver.Rows, error) {
		args := c.args[len(c.args)-1]
		if strings.HasPrefix(q, "SELECT max(ledger_seq) FROM stellar.ledgers") {
			return &stubRows{data: [][]any{{tip}}}, nil
		}
		var hi, lo uint32
		var limit int
		switch {
		case strings.Contains(q, "(SELECT max(ledger_seq) FROM stellar.ledgers) - ?"):
			hi, lo, limit = tip, tip-args[0].(uint32)+1, args[1].(int)
		case strings.Contains(q, "ledger_seq < ? AND ledger_seq >= ?"):
			hi, lo, limit = args[0].(uint32)-1, args[1].(uint32), args[2].(int)
		case strings.Contains(q, "ledger_seq <= ? AND ledger_seq >= ?"):
			hi, lo, limit = args[0].(uint32), args[1].(uint32), args[2].(int)
		default:
			t.Fatalf("unexpected query: %s", q)
		}
		if hi > tip {
			hi = tip
		}
		var data [][]any
		for s := uint64(hi); s >= uint64(lo) && s > 0 && len(data) < limit; s-- {
			if present(uint32(s)) {
				data = append(data, stubLedgerRow(uint32(s)))
			}
		}
		return &stubRows{data: data}, nil
	}
	return c
}

// stubLedgerRow is one scanLedger row; only Seq matters to these tests.
func stubLedgerRow(seq uint32) []any {
	return []any{seq, time.Time{}, "", "", uint32(0), uint32(0), uint32(0), uint32(0), int64(0), int64(0), uint32(0), uint32(0)}
}

func seqs(ls []LedgerHeader) []uint32 {
	out := make([]uint32, len(ls))
	for i, l := range ls {
		out[i] = l.Seq
	}
	return out
}

func wantSeqs(t *testing.T, got []LedgerHeader, from uint32, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("got %d ledgers %v, want %d descending from %d", len(got), seqs(got), n, from)
	}
	for i, l := range got {
		if l.Seq != from-uint32(i) {
			t.Fatalf("ledger[%d] = %d, want %d (page %v)", i, l.Seq, from-uint32(i), seqs(got))
		}
	}
}

// A cursor landing just above a lake hole wider than the tail window must
// page across it, not return an empty page that ends pagination.
func TestRecentLedgers_CursorPageSpansHole(t *testing.T) {
	conn := ledgerLakeStub(t, func(s uint32) bool { return s < 80_000 || s >= 99_990 }, 100_000)
	r := &ExplorerReader{conn: conn}
	got, err := r.RecentLedgers(context.Background(), 8, 99_990)
	if err != nil {
		t.Fatal(err)
	}
	wantSeqs(t, got, 79_999, 8)
}

func TestRecentLedgers_TipPageSpansHole(t *testing.T) {
	conn := ledgerLakeStub(t, func(s uint32) bool { return s <= 50_000 || s >= 99_997 }, 100_000)
	r := &ExplorerReader{conn: conn}
	got, err := r.RecentLedgers(context.Background(), 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{100_000, 99_999, 99_998, 99_997, 50_000, 49_999, 49_998, 49_997}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", seqs(got), want)
	}
	for i := range want {
		if got[i].Seq != want[i] {
			t.Fatalf("got %v, want %v", seqs(got), want)
		}
	}
}

// ?before= far above the tip must collapse to the tip page in a small,
// fixed number of reads — never a widening walk down to a whole-table read.
func TestRecentLedgers_BeforeAboveTipIsBounded(t *testing.T) {
	conn := ledgerLakeStub(t, func(uint32) bool { return true }, 100_000)
	r := &ExplorerReader{conn: conn}
	got, err := r.RecentLedgers(context.Background(), 8, math.MaxUint32)
	if err != nil {
		t.Fatal(err)
	}
	wantSeqs(t, got, 100_000, 8)
	if len(conn.queries) > 3 {
		t.Fatalf("issued %d queries, want <= 3: %v", len(conn.queries), conn.queries)
	}
	for i, q := range conn.queries {
		if strings.Contains(q, "ledger_seq >= ?") && conn.args[i][1].(uint32) == 0 {
			t.Fatalf("query %d reads down to ledger 0 though the tail fills the page", i)
		}
	}
}

// The contiguous hot path stays a single bounded read.
func TestRecentLedgers_ContiguousIsOneQuery(t *testing.T) {
	for _, before := range []uint32{0, 90_000} {
		conn := ledgerLakeStub(t, func(uint32) bool { return true }, 100_000)
		r := &ExplorerReader{conn: conn}
		got, err := r.RecentLedgers(context.Background(), 50, before)
		if err != nil {
			t.Fatal(err)
		}
		from := uint32(100_000)
		if before > 0 {
			from = before - 1
		}
		wantSeqs(t, got, from, 50)
		if len(conn.queries) != 1 {
			t.Fatalf("before=%d: %d queries, want 1", before, len(conn.queries))
		}
	}
}
