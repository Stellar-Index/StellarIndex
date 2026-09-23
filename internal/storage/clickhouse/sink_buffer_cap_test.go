package clickhouse

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TestSinkBufferCapBoundedDrop verifies the G12-01 bounded-drop: once the
// in-memory buffer reaches maxBufferLedgers (a sustained-CH-outage proxy, since
// the cap is only reached when flushes are failing), Add DROPS the incoming
// extract with ErrBufferFull instead of growing the buffer unbounded.
//
// This exercises the cap purely in-memory: with maxBufferLedgers set, Add
// returns ErrBufferFull BEFORE any auto-flush could be triggered (cap < the
// flushEvery threshold here), so no ClickHouse connection is needed.
func TestSinkBufferCapBoundedDrop(t *testing.T) {
	s := &Sink{flushEvery: 1_000_000} // never auto-flush during the test
	s.SetMaxBufferLedgers(3)

	ctx := context.Background()
	ext := func(seq uint32) LedgerExtract {
		return LedgerExtract{Ledger: LedgerRow{LedgerSeq: seq}}
	}

	// First 3 fit under the cap.
	for i := uint32(1); i <= 3; i++ {
		if err := s.Add(ctx, ext(i)); err != nil {
			t.Fatalf("Add(%d) under cap: unexpected error %v", i, err)
		}
	}
	if got := s.BufferedLedgers(); got != 3 {
		t.Fatalf("BufferedLedgers after 3 adds = %d, want 3", got)
	}

	// The 4th and 5th exceed the cap → bounded-drop, buffer stays at 3.
	for i := uint32(4); i <= 5; i++ {
		err := s.Add(ctx, ext(i))
		if !errors.Is(err, ErrBufferFull) {
			t.Fatalf("Add(%d) over cap = %v, want ErrBufferFull", i, err)
		}
	}
	if got := s.BufferedLedgers(); got != 3 {
		t.Fatalf("BufferedLedgers stayed bounded? = %d, want 3 (no unbounded growth)", got)
	}
}

// TestSinkBufferCapUnboundedByDefault verifies the cap is opt-in: a Sink with no
// SetMaxBufferLedgers (the backfill default) never returns ErrBufferFull, because
// backfill callers retry the SAME range on flush failure rather than streaming
// new ledgers on top.
func TestSinkBufferCapUnboundedByDefault(t *testing.T) {
	s := &Sink{flushEvery: 1_000_000}
	ctx := context.Background()
	for i := uint32(1); i <= 100; i++ {
		if err := s.Add(ctx, LedgerExtract{Ledger: LedgerRow{LedgerSeq: i}}); err != nil {
			t.Fatalf("Add(%d) with no cap: unexpected error %v", i, err)
		}
	}
	if got := s.BufferedLedgers(); got != 100 {
		t.Fatalf("BufferedLedgers = %d, want 100 (unbounded by default)", got)
	}
}

// fakeOrderConn records which stellar.* table each PrepareBatch call targets,
// in call order. It embeds driver.Conn so any method Flush doesn't exercise
// panics loudly instead of silently satisfying the interface with a zero
// value (see the Conn doc comment on hand-implementing it).
type fakeOrderConn struct {
	driver.Conn
	tables []string

	// failTable, when set, makes that table's batch fail: at PrepareBatch if
	// failAtPrepare, otherwise at Send. It models a partial flush.
	failTable     string
	failAtPrepare bool
}

var errInjected = errors.New("injected clickhouse failure")

func (c *fakeOrderConn) PrepareBatch(_ context.Context, query string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	table := tableFromInsert(query)
	c.tables = append(c.tables, table)
	if table == c.failTable {
		if c.failAtPrepare {
			return nil, errInjected
		}
		return fakeBatch{sendErr: errInjected}, nil
	}
	return fakeBatch{}, nil
}

// tableFromInsert extracts the table name from a `INSERT INTO stellar.X (...)`
// query string, the shape every flush* method in sink.go uses.
func tableFromInsert(query string) string {
	const prefix = "INSERT INTO stellar."
	i := strings.Index(query, prefix)
	if i < 0 {
		return query
	}
	rest := query[i+len(prefix):]
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		rest = rest[:sp]
	}
	return rest
}

// fakeBatch discards every appended row; only the PrepareBatch call order is
// under test.
type fakeBatch struct {
	driver.Batch
	sendErr error
}

func (fakeBatch) Append(...any) error { return nil }
func (b fakeBatch) Send() error       { return b.sendErr }

// fullSink returns a Sink over conn with one row buffered in every table, and
// fails the test if any slice buffer on Sink is left empty — so a buffer added
// later cannot dodge the flush-order tests by being skipped-when-empty.
func fullSink(t *testing.T, conn driver.Conn) *Sink {
	t.Helper()
	s := &Sink{conn: conn}
	s.ledgers = []LedgerRow{{LedgerSeq: 1}}
	s.txs = []TransactionRow{{}}
	s.ops = []OperationRow{{}}
	s.results = []OperationResultRow{{}}
	s.participants = []OperationParticipantRow{{}}
	s.events = []ContractEventRow{{}}
	s.changes = []LedgerEntryChangeRow{{}}
	s.supplyFlows = []SupplyFlowRow{{}}

	v := reflect.ValueOf(s).Elem()
	for i := 0; i < v.NumField(); i++ {
		if f := v.Field(i); f.Kind() == reflect.Slice && f.Len() == 0 {
			t.Fatalf("Sink buffer %q is empty in fullSink; populate it so its flush is ordered against ledgers", v.Type().Field(i).Name)
		}
	}
	return s
}

// TestSinkFlushOrderLedgersLast pins the K074 invariant declared in the
// "ORDERING IS LOAD-BEARING" comment on Flush: stellar.ledgers must be the
// LAST table flushed, after every table the completeness watermark
// (ContiguousWatermark) depends on, so a present ledgers row is a valid
// per-ledger commit marker. It observes the REAL PrepareBatch call order
// Flush issues, not a hand-maintained mirror of it.
func TestSinkFlushOrderLedgersLast(t *testing.T) {
	conn := &fakeOrderConn{}
	s := fullSink(t, conn)

	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: unexpected error %v", err)
	}

	if len(conn.tables) == 0 {
		t.Fatal("Flush issued no PrepareBatch calls")
	}
	last := conn.tables[len(conn.tables)-1]
	if last != "ledgers" {
		t.Fatalf("Flush's last INSERT targeted %q, want %q — ledgers must be flushed last as the commit marker", last, "ledgers")
	}
	for i, table := range conn.tables[:len(conn.tables)-1] {
		if table == "ledgers" {
			t.Fatalf("ledgers flushed at position %d of %d (before other tables); ledgers must be LAST", i, len(conn.tables))
		}
	}
}

// TestSinkFlushPartialFailureWritesNoCommitMarker pins the other half of the
// commit-marker invariant: when any other table's batch fails, Flush must stop
// before stellar.ledgers, return the error and keep every buffer for the retry.
// Ordering alone does not give this — a Flush that collected errors and carried
// on would still write ledgers last, and ContiguousWatermark would then read a
// ledger whose events or changes never landed as complete.
func TestSinkFlushPartialFailureWritesNoCommitMarker(t *testing.T) {
	baseline := &fakeOrderConn{}
	if err := fullSink(t, baseline).Flush(context.Background()); err != nil {
		t.Fatalf("baseline Flush: %v", err)
	}
	for _, table := range baseline.tables {
		if table == "ledgers" {
			continue
		}
		for _, atPrepare := range []bool{false, true} {
			conn := &fakeOrderConn{failTable: table, failAtPrepare: atPrepare}
			s := fullSink(t, conn)
			err := s.Flush(context.Background())
			if !errors.Is(err, errInjected) {
				t.Fatalf("fail %s (atPrepare=%v): Flush = %v, want the injected error", table, atPrepare, err)
			}
			if slices.Contains(conn.tables, "ledgers") {
				t.Fatalf("fail %s (atPrepare=%v): Flush still wrote stellar.ledgers %v, a commit marker for a ledger whose %s never landed", table, atPrepare, conn.tables, table)
			}
			if got := s.BufferedLedgers(); got != 1 {
				t.Fatalf("fail %s (atPrepare=%v): BufferedLedgers = %d, want 1 (buffers kept for retry)", table, atPrepare, got)
			}
		}
	}
}
