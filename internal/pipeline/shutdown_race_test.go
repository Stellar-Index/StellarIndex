package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
)

// shutdownRacedTradeStore models a steady-state batch write that is IN
// FLIGHT when the parent ctx is cancelled — the pipeline sibling of the
// sorobanevents race fixed in #240. The first BatchInsertTrades parks
// until its ctx is cancelled and returns ctx.Err(), exactly as a real
// pgx INSERT does when its context dies mid-statement; every call
// (batch or row) honours an already-dead ctx the same way pgx does.
// Later calls under a live ctx land. Deterministic: nothing depends on
// which of two ready select cases the scheduler picks.
type shutdownRacedTradeStore struct {
	entered chan struct{} // closed when the first (in-flight) batch write starts

	mu         sync.Mutex
	landed     []canonical.Trade
	batchCalls int
	rowCalls   int
}

func newShutdownRacedTradeStore() *shutdownRacedTradeStore {
	return &shutdownRacedTradeStore{entered: make(chan struct{})}
}

func (s *shutdownRacedTradeStore) BatchInsertTrades(ctx context.Context, trades []canonical.Trade) error {
	s.mu.Lock()
	s.batchCalls++
	first := s.batchCalls == 1
	s.mu.Unlock()
	if first {
		close(s.entered)
		<-ctx.Done()
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.landed = append(s.landed, trades...)
	s.mu.Unlock()
	return nil
}

func (s *shutdownRacedTradeStore) InsertTrade(ctx context.Context, t canonical.Trade) error {
	s.mu.Lock()
	s.rowCalls++
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.landed = append(s.landed, t)
	s.mu.Unlock()
	return nil
}

func (s *shutdownRacedTradeStore) WouldPopulateUSDVolume(context.Context, canonical.Trade) bool {
	return false
}

func (s *shutdownRacedTradeStore) landedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.landed)
}

func (s *shutdownRacedTradeStore) calls() (batch, row int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batchCalls, s.rowCalls
}

// TestPersistWorker_ShutdownRacingInFlightTradeFlush_RowsLandNotLost pins
// the pipeline instance of the shutdown data-loss class fixed for the
// sorobanevents AsyncSink in #240: a steady-state trade batch whose
// BatchInsertTrades is in flight when the parent ctx is cancelled.
//
// Mechanism (pre-fix): persistWorker's ticker flush runs under the
// parent ctx (shutdownSafeCtx passes a live ctx straight through). The
// cancel makes the in-flight write return context.Canceled, which
// timescale.IsInfraError deliberately does NOT classify as infra, so
// flushTradeBatch fell into per-row isolation — every InsertTrade then
// failed instantly against the same dead ctx and persistTrade logged
// each row as "abandoned on shutdown — re-derive". Up to tradeBatchSize
// already-accepted trades were lost on every deploy that caught a flush
// mid-flight, while the worker's own flushShutdown (a FRESH ctx bounded
// by drainTimeout) ran a moment later with an empty tradeBuf.
//
// The interrupted batch must instead be carried into flushShutdown and
// land there. Proven red on the pre-fix code: landed 0, want 3.
func TestPersistWorker_ShutdownRacingInFlightTradeFlush_RowsLandNotLost(t *testing.T) {
	droppedBefore := counter(t, obs.SourceInsertErrorsTotal, "sdex", "trade")

	store := newShutdownRacedTradeStore()
	in := make(chan consumer.Event, 16)
	const total = 3
	for i := 0; i < total; i++ {
		in <- sdex.TradeEvent{Trade: mkTrade("sdex", uint32(7_000_000+i))}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// workerID 1: not the blocking-drain worker, so the worker exits
		// straight after its own shutdown flush. A nil-store event
		// persister is fine — only trade-shaped events flow, and they go
		// through tw.
		persistWorker(ctx, discardLogger(), storeEventPersister(discardLogger(), nil), store, in, SinkModeAll, 1, nil)
	}()

	// The ticker (tradeBatchFlushInterval) flushes the 3 buffered trades;
	// wait until that write is in flight, then cancel while it is parked
	// — the exact race a SIGTERM lands on.
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first batch write never started")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("persistWorker did not return after ctx cancel")
	}

	if got := store.landedCount(); got != total {
		t.Errorf("landed %d trades, want %d — the shutdown-interrupted steady-state batch was not carried into the worker's shutdown flush", got, total)
	}
	if batch, _ := store.calls(); batch < 2 {
		t.Errorf("BatchInsertTrades called %d times, want >= 2 (the cancelled attempt + the shutdown-flush retry)", batch)
	}
	if got := counter(t, obs.SourceInsertErrorsTotal, "sdex", "trade") - droppedBefore; got != 0 {
		t.Errorf("source_insert_errors{sdex,trade} delta = %v, want 0 — rows already accepted must not be counted lost because shutdown raced their flush", got)
	}
}

// TestFlushTradeBatch_CtxCancelledMidWrite_ReturnsWholeBatch pins the
// contract the carry depends on: a batch write that fails with the
// ctx's own cancellation is handed back to the caller in full — not
// isolated per-row against the dead ctx (which can only fail every
// row instantly and count each one lost).
func TestFlushTradeBatch_CtxCancelledMidWrite_ReturnsWholeBatch(t *testing.T) {
	droppedBefore := counter(t, obs.SourceInsertErrorsTotal, "sdex", "trade")

	store := newShutdownRacedTradeStore()
	batch := []canonical.Trade{mkTrade("sdex", 10), mkTrade("sdex", 11), mkTrade("sdex", 12)}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-store.entered
		cancel()
	}()
	got := flushTradeBatch(ctx, discardLogger(), store, nil, batch, 0)

	if len(got) != len(batch) {
		t.Fatalf("flushTradeBatch returned %d trades, want the whole batch of %d", len(got), len(batch))
	}
	for i := range batch {
		if got[i].Ledger != batch[i].Ledger {
			t.Errorf("returned[%d].Ledger = %d, want %d", i, got[i].Ledger, batch[i].Ledger)
		}
	}
	if n := store.landedCount(); n != 0 {
		t.Errorf("landed %d, want 0 (the write was cancelled)", n)
	}
	if _, rows := store.calls(); rows != 0 {
		t.Errorf("InsertTrade called %d times, want 0 — a ctx-cancelled batch must not be isolated per-row against the dead ctx", rows)
	}
	if got := counter(t, obs.SourceInsertErrorsTotal, "sdex", "trade") - droppedBefore; got != 0 {
		t.Errorf("source_insert_errors{sdex,trade} delta = %v, want 0", got)
	}
}

// shutdownRacedEventPersister models a steady-state NON-trade served-tier
// write that is IN FLIGHT when the parent ctx is cancelled — the
// non-trade sibling of [shutdownRacedTradeStore]. The first call parks
// until its ctx is cancelled and returns ctx.Err(), exactly as a real
// pgx INSERT does when its context dies mid-statement; later calls under
// a live ctx land. Deterministic: nothing depends on which of two ready
// select cases the scheduler picks.
type shutdownRacedEventPersister struct {
	entered chan struct{} // closed when the first (in-flight) write starts

	mu     sync.Mutex
	landed []consumer.Event
	calls  int
}

func newShutdownRacedEventPersister() *shutdownRacedEventPersister {
	return &shutdownRacedEventPersister{entered: make(chan struct{})}
}

func (p *shutdownRacedEventPersister) persist(ctx context.Context, ev consumer.Event, _ bool) error {
	p.mu.Lock()
	p.calls++
	first := p.calls == 1
	p.mu.Unlock()
	if first {
		close(p.entered)
		<-ctx.Done()
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.landed = append(p.landed, ev)
	p.mu.Unlock()
	return nil
}

func (p *shutdownRacedEventPersister) landedEvents() []consumer.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]consumer.Event(nil), p.landed...)
}

// TestPersistWorker_ShutdownRacingInFlightEventWrite_EventLandsNotLost is
// the NON-TRADE half of the shutdown data-loss class (#368 M3). Its trade
// twin is TestPersistWorker_ShutdownRacingInFlightTradeFlush_RowsLandNotLost
// above; the same race on the same select arm was still open for
// everything that is not trade-shaped.
//
// Mechanism (pre-fix): persistWorker's `<-in` arm dequeued a non-trade
// event and wrote it under the parent ctx (shutdownSafeCtx passes a LIVE
// ctx straight through — it only swaps in a fresh one when ctx is
// ALREADY dead). A SIGTERM landing while that write was in flight made
// it return context.Canceled, retryInfra classified that as
// faultShutdown and gave up, and persistEventResilient logged the event
// "abandoned on shutdown" and counted it dropped. The event was already
// OFF the channel, so no later drain pass could see it and nothing would
// ever redeliver it — while the worker's whole drain budget
// (drainTimeout) sat unused a moment later. These are served-tier writes
// NOBODY else makes (band oracle_updates, external.UpdateEvent, the
// supply observers' LedgerEntry observations, soroswap_router swaps,
// defindex flows), and their cursor had already advanced.
//
// The interrupted event must instead be CARRIED into the shutdown pass
// and land there, exactly as the interrupted trade batch is. Proven red
// on the pre-fix code: landed 0 events, want 1, and dropped +1.
func TestPersistWorker_ShutdownRacingInFlightEventWrite_EventLandsNotLost(t *testing.T) {
	droppedBefore := counter(t, obs.SourceInsertErrorsTotal, band.SourceName, "dropped")

	ep := newShutdownRacedEventPersister()
	in := make(chan consumer.Event, 4)
	// Non-trade, non-projected served-tier write: tradeFromEvent does not
	// claim it, so it takes the persistEventResilient branch of the `<-in`
	// arm — the branch under test.
	want := band.UpdateEvent{Update: canonical.OracleUpdate{
		Source: band.SourceName,
		Ledger: 7_100_001,
		TxHash: "band-tx",
	}}
	in <- want

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// workerID 1: not the blocking-drain worker, so the worker exits
		// straight after its own shutdown pass.
		persistWorker(ctx, discardLogger(), ep.persist, nil, in, SinkModeAll, 1, nil)
	}()

	// Wait until the steady-state write is in flight, then cancel while it
	// is parked — the exact race a SIGTERM lands on.
	select {
	case <-ep.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first event write never started")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("persistWorker did not return after ctx cancel")
	}

	got := ep.landedEvents()
	if len(got) != 1 {
		t.Fatalf("landed %d event(s), want 1 — the shutdown-interrupted steady-state write was not carried into the worker's shutdown pass", len(got))
	}
	landedBand, ok := got[0].(band.UpdateEvent)
	if !ok {
		t.Fatalf("landed event has type %T, want band.UpdateEvent", got[0])
	}
	if landedBand.Update.Ledger != want.Update.Ledger || landedBand.Update.TxHash != want.Update.TxHash {
		t.Errorf("landed event = ledger %d / tx %q, want ledger %d / tx %q — a DIFFERENT event was carried",
			landedBand.Update.Ledger, landedBand.Update.TxHash, want.Update.Ledger, want.Update.TxHash)
	}

	// The carry must not also be reported as a loss: an event that landed
	// is not a drop, and stellarindex_source_insert_errors_total{kind=
	// "dropped"} is what an operator re-derives a source's tail on.
	if got, before := counter(t, obs.SourceInsertErrorsTotal, band.SourceName, "dropped"), droppedBefore; got != before {
		t.Errorf("SourceInsertErrorsTotal{source=band,kind=dropped} moved %v -> %v; the carried event landed, so nothing was dropped", before, got)
	}
}
