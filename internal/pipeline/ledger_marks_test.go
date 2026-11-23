package pipeline

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
)

// keyedStore is a served tier keyed like the real ON CONFLICT tables: a row written twice is still one row. Writes of
// stallKey park until the process "dies"; after death every write fails as the dropped connection's ctx would.
type keyedStore struct {
	stallKey string
	dead     chan struct{}

	mu       sync.Mutex
	rows     map[string]bool
	attempts map[string]int
}

func newKeyedStore(stallKey string, rows map[string]bool) *keyedStore {
	if rows == nil {
		rows = map[string]bool{}
	}
	return &keyedStore{stallKey: stallKey, dead: make(chan struct{}), rows: rows, attempts: map[string]int{}}
}

func (s *keyedStore) write(ctx context.Context, key string) error {
	s.mu.Lock()
	s.attempts[key]++
	s.mu.Unlock()
	select {
	case <-s.dead:
		return context.Canceled
	default:
	}
	if key == s.stallKey {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.dead:
			return context.Canceled
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[key] = true
	return nil
}

func (s *keyedStore) persist(ctx context.Context, ev consumer.Event, _ bool) error {
	u := ev.(band.UpdateEvent).Update
	return s.write(ctx, "oracle:"+u.TxHash)
}

func (s *keyedStore) BatchInsertTrades(ctx context.Context, trades []canonical.Trade) error {
	for _, t := range trades {
		if err := s.write(ctx, "trade:"+t.TxHash); err != nil {
			return err
		}
	}
	return nil
}

func (s *keyedStore) InsertTrade(ctx context.Context, t canonical.Trade) error {
	return s.write(ctx, "trade:"+t.TxHash)
}

func (*keyedStore) WouldPopulateUSDVolume(context.Context, canonical.Trade) bool { return false }

func (s *keyedStore) snapshot() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.rows))
	for k := range s.rows {
		out[k] = true
	}
	return out
}

func (s *keyedStore) attempted(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[key] > 0
}

func (s *keyedStore) has(keys ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		if !s.rows[k] {
			return false
		}
	}
	return true
}

// testLedger is what the dispatcher would emit for ledger l: an oracle update and/or a trade, keyed by ledger.
func testLedger(l uint32, oracle, trade bool) []consumer.Event {
	var out []consumer.Event
	if oracle {
		out = append(out, band.UpdateEvent{Update: canonical.OracleUpdate{Source: band.SourceName, Ledger: l, TxHash: fmt.Sprint(l)}})
	}
	if trade {
		tr := mkTrade("sdex", l)
		tr.TxHash = fmt.Sprint(l)
		out = append(out, sdex.TradeEvent{Trade: tr})
	}
	return out
}

var killTestLedgers = map[uint32][]consumer.Event{
	101: testLedger(101, true, false),
	102: testLedger(102, false, true),
	103: testLedger(103, true, true), // its oracle write is in flight when the process dies
	104: testLedger(104, false, true),
	105: testLedger(105, true, false),
}

type laneRun struct {
	in     chan consumer.Event
	cancel context.CancelFunc
	done   chan struct{}
}

func startLane(store *keyedStore) *laneRun {
	ctx, cancel := context.WithCancel(context.Background())
	r := &laneRun{in: make(chan consumer.Event, 64), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		runSinkLane(ctx, discardLogger(), store.persist, store, r.in, SinkModeAll, PersistWorkers, nil)
	}()
	return r
}

func (r *laneRun) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	close(r.in)
	select {
	case <-r.done:
	case <-time.After(30 * time.Second):
		t.Fatal("sink lane did not stop")
	}
}

// A process dies with ledger 103's oracle row in flight (and any trade its worker had buffered) while later ledgers
// have landed. The resume cursor (the durable mark) must sit just below the lowest unlanded row, so the restart
// re-walks from there: every row ends up present exactly once and nothing at or below the cursor is re-written. Under
// advance-on-enqueue the cursor read 105 and the parked rows were lost for good.
func TestLedgerMarks_KillMidBatchRestartRederivesUnlandedRows(t *testing.T) {
	store := newKeyedStore("oracle:103", nil)
	marks := NewLedgerMarks(100)
	run := startLane(store)
	ctx := context.Background()
	for l := uint32(101); l <= 105; l++ {
		if err := enqueueLedger(ctx, run.in, killTestLedgers[l], l, marks); err != nil {
			t.Fatal(err)
		}
	}
	// 103's oracle write is in flight and parks its worker, together with any trade that worker had buffered but not
	// yet flushed. Let every other worker land and flush.
	waitFor(t, func() bool { return store.has("oracle:101", "oracle:105") && store.attempted("oracle:103") })
	time.Sleep(3 * tradeBatchFlushInterval)

	// Abrupt stop: whatever was durable at this instant is all the next process sees.
	survived := store.snapshot()
	unlanded := uint32(0)
	for _, k := range killTestKeys {
		if !survived[k.key] && (unlanded == 0 || k.ledger < unlanded) {
			unlanded = k.ledger
		}
	}
	cursor := marks.DurableThrough()
	if marks.Processed() != 105 {
		t.Fatalf("processed = %d, want 105", marks.Processed())
	}
	if unlanded == 0 || unlanded > 103 {
		t.Fatalf("lowest unlanded ledger = %d, want <= 103 (the parked write)", unlanded)
	}
	if cursor != unlanded-1 {
		t.Fatalf("durable mark at the crash = %d, want %d: landed %v", cursor, unlanded-1, survived)
	}

	close(store.dead)
	run.stop(t)
	if got := marks.DurableThrough(); got != cursor {
		t.Fatalf("durable mark moved to %d after the crash, want %d", got, cursor)
	}

	restarted := newKeyedStore("", survived)
	marks2 := NewLedgerMarks(cursor)
	run2 := startLane(restarted)
	for l := cursor + 1; l <= 105; l++ {
		if err := enqueueLedger(ctx, run2.in, killTestLedgers[l], l, marks2); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return marks2.DurableThrough() == 105 })
	run2.stop(t)

	got := restarted.snapshot()
	for _, k := range killTestKeys {
		if !got[k.key] {
			t.Errorf("row %s missing after restart", k.key)
		}
		if k.ledger <= cursor && restarted.attempts[k.key] != 0 {
			t.Errorf("restart re-wrote %s, at or below the cursor %d", k.key, cursor)
		}
	}
	if len(got) != len(killTestKeys) {
		t.Errorf("served tier holds %d rows, want %d: %v", len(got), len(killTestKeys), got)
	}
}

var killTestKeys = []struct {
	key    string
	ledger uint32
}{
	{"oracle:101", 101}, {"trade:102", 102}, {"oracle:103", 103}, {"trade:103", 103}, {"trade:104", 104}, {"oracle:105", 105},
}

// After cancel the workers stop acking, so rows the shutdown drain lands do not move the mark; only a drain that
// finished with nothing abandoned may declare every processed ledger durable (main's clean-shutdown commit).
func TestLedgerMarks_ShutdownDrainFreezesMarkUntilAllLanded(t *testing.T) {
	store := newKeyedStore("", nil)
	marks := NewLedgerMarks(200)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the shutdown signal already arrived: every row below lands through a drain path
	in := make(chan consumer.Event, 8)
	if err := enqueueLedger(context.Background(), in, testLedger(201, true, true), 201, marks); err != nil {
		t.Fatal(err)
	}
	close(in)
	loss := runSinkLane(ctx, discardLogger(), store.persist, store, in, SinkModeAll, PersistWorkers, nil)

	if !store.has("oracle:201", "trade:201") {
		t.Fatalf("drain did not land ledger 201: %v", store.snapshot())
	}
	if got := marks.DurableThrough(); got != 200 {
		t.Fatalf("durable mark = %d after a post-cancel drain, want 200 (frozen)", got)
	}
	if loss.Rows != 0 {
		t.Fatalf("loss = %+v, want none", loss)
	}
	marks.AllLanded()
	if got := marks.DurableThrough(); got != 201 {
		t.Fatalf("durable mark = %d after AllLanded, want 201", got)
	}
}

// The mark never passes the lowest ledger with an outstanding row, whatever order rows land in.
func TestLedgerMarks_DurableThroughIsMinOutstanding(t *testing.T) {
	m := NewLedgerMarks(9)
	for _, l := range []uint32{10, 10, 11, 12} {
		m.enqueued(l)
	}
	m.finished(12)
	m.landed(12)
	m.landed(10)
	if got := m.DurableThrough(); got != 9 {
		t.Fatalf("DurableThrough = %d, want 9 (one row of 10 outstanding)", got)
	}
	m.landed(10)
	if got := m.DurableThrough(); got != 10 {
		t.Fatalf("DurableThrough = %d, want 10 (11 outstanding)", got)
	}
	m.landed(11)
	if got := m.DurableThrough(); got != 12 {
		t.Fatalf("DurableThrough = %d, want 12", got)
	}
	m.enqueued(13) // 13 partially enqueued: not processed, so not durable
	if got := m.DurableThrough(); got != 12 {
		t.Fatalf("DurableThrough = %d, want 12 while 13 is mid-enqueue", got)
	}
}
