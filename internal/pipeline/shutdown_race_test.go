package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
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
		// straight after its own shutdown flush. nil *timescale.Store is
		// fine — only trade-shaped events flow, and they go through tw.
		persistWorker(ctx, discardLogger(), nil, store, in, SinkModeAll, 1, nil)
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
