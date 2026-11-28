package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// cancelRacedStore is a healthy store whose first batch write is in flight when the lane is cancelled and fails the
// way Postgres answers pgx's cancel request: 57014 query_canceled, not a ctx error, a moment after the cancel.
type cancelRacedStore struct {
	fakeTradeStore
	once    sync.Once
	entered chan struct{}
}

func (s *cancelRacedStore) BatchInsertTrades(ctx context.Context, trades []canonical.Trade) error {
	first := false
	s.once.Do(func() { first = true })
	if first {
		close(s.entered)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // the cancel request's round trip
		return &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.fakeTradeStore.BatchInsertTrades(ctx, trades)
}

func (s *cancelRacedStore) InsertTrade(ctx context.Context, t canonical.Trade) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.fakeTradeStore.InsertTrade(ctx, t)
}

// blipStore fails its first batch write with an infra fault, then recovers.
type blipStore struct {
	fakeTradeStore
	once sync.Once
}

func (s *blipStore) BatchInsertTrades(ctx context.Context, trades []canonical.Trade) error {
	failed := false
	s.once.Do(func() { failed = true })
	if failed {
		return errInfra
	}
	return s.fakeTradeStore.BatchInsertTrades(ctx, trades)
}

// A fault that clears inside the budget is not a loss: the final drain retries until its deadline.
func TestExternalRetryBuffer_FinalDrainRetriesWithinBudget(t *testing.T) {
	store := &blipStore{}
	store.healthy.Store(true)
	buf := newExternalRetryBuffer(store, discardLogger(), 8)
	buf.enqueue(mkTrade("coinbase", 1))
	buf.enqueue(mkTrade("coinbase", 2))
	lt := &lossTracker{}

	buf.finalDrain(time.Now().Add(5*time.Second), lt)

	if got := store.landedCount(); got != 2 {
		t.Errorf("landed = %d; want 2 (the second attempt is inside the budget)", got)
	}
	if loss := lt.snapshot(); loss.Rows != 0 {
		t.Errorf("loss = %+v; want none", loss)
	}
}

// A store that never answers holds the final drain only to its deadline; what is left is counted and named.
func TestExternalRetryBuffer_FinalDrainStopsAtDeadline(t *testing.T) {
	before := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade")
	buf := newExternalRetryBuffer(&hangingTradeStore{}, discardLogger(), 8)
	buf.enqueue(mkTrade("coinbase", 1))
	buf.enqueue(mkTrade("coinbase", 2))
	lt := &lossTracker{}

	start := time.Now()
	buf.finalDrain(start.Add(50*time.Millisecond), lt)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("final drain took %v; want it to stop at its 50ms deadline", elapsed)
	}
	if loss := lt.snapshot(); loss.Rows != 2 {
		t.Errorf("loss = %+v; want the 2 unwritten rows", loss)
	}
	if got := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade") - before; got != 2 {
		t.Errorf("undrained trade rows delta = %v; want 2", got)
	}
}

// Venue trades the lane accepted just before shutdown, with Postgres up, must be written before the lane returns,
// once each, however the cancelled write hands them on: they have no cursor and nothing refetches them.
func TestRunSinkLane_TradesAcceptedBeforeShutdownAreWritten(t *testing.T) {
	store := &cancelRacedStore{entered: make(chan struct{})}
	store.healthy.Store(true)
	before := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade")
	in := make(chan consumer.Event, 8)
	for ledger := uint32(1); ledger <= 4; ledger++ {
		in <- external.TradeEvent{Trade: mkTrade("coinbase", ledger)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan ShutdownLoss, 1)
	go func() {
		done <- runSinkLane(ctx, discardLogger(), func(context.Context, consumer.Event, bool) error { return nil },
			boundedTradeWriter{store, externalWriteTimeout}, in, SinkModeAll, ExternalPersistWorkers,
			newExternalRetryBuffer(store, discardLogger(), 0))
	}()
	<-store.entered // the steady-state flush is in flight
	cancel()
	close(in)

	var loss ShutdownLoss
	select {
	case loss = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("lane did not return")
	}
	if loss.Rows != 0 {
		t.Errorf("loss = %+v; want none, the store was up", loss)
	}
	if got := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade") - before; got != 0 {
		t.Errorf("undrained trade rows delta = %v; want 0", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	seen := map[uint32]int{}
	for _, tr := range store.landed {
		seen[tr.Ledger]++
	}
	for ledger := uint32(1); ledger <= 4; ledger++ {
		if seen[ledger] != 1 {
			t.Errorf("trade %d written %d times; want exactly once (landed %d rows)", ledger, seen[ledger], len(store.landed))
		}
	}
}
