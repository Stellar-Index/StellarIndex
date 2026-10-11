package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// shutdownRacedTradeStore models a batch write IN FLIGHT when the parent ctx is cancelled: the first BatchInsertTrades parks until its ctx is cancelled and returns ctx.Err(), as pgx does;
// every call honours an already-dead ctx; later calls under a live ctx land. Deterministic regardless of which select case the scheduler picks.
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

// TestPersistWorker_ShutdownRacingInFlightTradeFlush_RowsLandNotLost: a ticker flush interrupted by cancel returns context.Canceled, which is not infra,
// so without the carry every row would be isolated against the dead ctx and logged lost. The interrupted batch must land via flushShutdown (red without the fix: landed 0, want 3).
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
		persistWorker(ctx, discardLogger(), storeEventPersister(discardLogger(), nil), store, in, SinkModeAll, 1, nil, nil)
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

// shutdownRacedEventPersister is the non-trade sibling of shutdownRacedTradeStore: the first call parks until its ctx is cancelled and returns ctx.Err(); later calls under a live ctx land.
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

// TestPersistWorker_ShutdownRacingInFlightEventWrite_EventLandsNotLost: a non-trade event dequeued by the `<-in` arm and interrupted by cancel is already off the channel,
// so nothing redelivers it unless it is carried into the shutdown pass (red without the fix: landed 0, want 1, dropped +1). The trade twin is above.
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
		persistWorker(ctx, discardLogger(), ep.persist, nil, in, SinkModeAll, 1, nil, nil)
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

// TestPersistWorker_PostCancelPhasesShareOneDeadline: after cancel, a winning `<-in`/ticker arm and the Done arm must write under the SAME absolute deadline,
// or an extra arm pushes Done past main's ShutdownDeadline and carried rows die unreported. 50 runs make a pass without the fix vanishingly unlikely.
func TestPersistWorker_PostCancelPhasesShareOneDeadline(t *testing.T) {
	for run := 0; run < 50; run++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		in := make(chan consumer.Event, 8)
		for i := 0; i < cap(in); i++ {
			in <- band.UpdateEvent{Update: canonical.OracleUpdate{Source: band.SourceName}}
		}
		var mu sync.Mutex
		deadlines := map[time.Time]int{}
		ep := func(c context.Context, _ consumer.Event, _ bool) error {
			d, ok := c.Deadline()
			if !ok {
				t.Error("post-cancel write ran under a context with no deadline")
			}
			mu.Lock()
			deadlines[d]++
			mu.Unlock()
			time.Sleep(time.Millisecond)
			return nil
		}
		persistWorker(ctx, discardLogger(), ep, nil, in, SinkModeAll, 1, nil, nil)
		if len(deadlines) != 1 {
			t.Fatalf("run %d: post-cancel writes used %d distinct deadlines %v; want exactly 1 shared drain deadline", run, len(deadlines), deadlines)
		}
	}
}

// TestPersistWorker_Phase3ParallelWriteCountsOnlyInProjector: under SinkModeSkipSoleWriter both writers persist un-promoted projected events, so only the projector counts them;
// events only the dispatcher writes, and every event under SinkModeAll, still count once.
func TestPersistWorker_Phase3ParallelWriteCountsOnlyInProjector(t *testing.T) {
	cases := []struct {
		name          string
		mode          SinkMode
		wantProjected bool
	}{
		{"phase3_parallel", SinkModeSkipSoleWriter, false},
		{"no_projector", SinkModeAll, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			soroswapBefore := counter(t, obs.SourceEventsTotal, soroswap.SourceName)
			sdexBefore := counter(t, obs.SourceEventsTotal, sdex.SourceName)

			var mu sync.Mutex
			calls := map[string]persistCall{}
			ep := func(ctx context.Context, ev consumer.Event, countEvent bool) error {
				mu.Lock()
				defer mu.Unlock()
				calls[ev.EventKind()] = persistCall{ctx: ctx, countEvent: countEvent}
				return nil
			}
			tw := &fakeTradeStore{}
			tw.healthy.Store(true)

			in := make(chan consumer.Event, 4)
			in <- soroswap.TradeEvent{Trade: mkTrade(soroswap.SourceName, 1)}
			in <- sdex.TradeEvent{Trade: mkTrade(sdex.SourceName, 2)}
			in <- aquarius.KillEvent{}
			in <- band.UpdateEvent{}
			close(in)
			persistWorker(context.Background(), discardLogger(), ep, tw, in, tc.mode, 1, nil, nil)

			if got := tw.landedCount(); got != 2 {
				t.Fatalf("trades landed = %d, want 2 (the parallel write must still land)", got)
			}
			wantSoroswap := 0.0
			if tc.wantProjected {
				wantSoroswap = 1
			}
			if d := counter(t, obs.SourceEventsTotal, soroswap.SourceName) - soroswapBefore; d != wantSoroswap {
				t.Errorf("source_events_total{soroswap} delta = %v, want %v", d, wantSoroswap)
			}
			if d := counter(t, obs.SourceEventsTotal, sdex.SourceName) - sdexBefore; d != 1 {
				t.Errorf("source_events_total{sdex} delta = %v, want 1", d)
			}

			kill, ok := calls[aquarius.KillEvent{}.EventKind()]
			if !ok {
				t.Fatal("aquarius kill event never reached the persister")
			}
			if kill.countEvent != tc.wantProjected {
				t.Errorf("aquarius countEvent = %v, want %v", kill.countEvent, tc.wantProjected)
			}
			if got := entriesBumpReachesStore(kill.ctx); got != tc.wantProjected {
				t.Errorf("aquarius entries bump reaches source_entry_counts = %v, want %v", got, tc.wantProjected)
			}
			bandCall, ok := calls[band.UpdateEvent{}.EventKind()]
			if !ok {
				t.Fatal("band update never reached the persister")
			}
			if !bandCall.countEvent || !entriesBumpReachesStore(bandCall.ctx) {
				t.Errorf("band (dispatcher-only) countEvent = %v, entries counted = %v; want both true",
					bandCall.countEvent, entriesBumpReachesStore(bandCall.ctx))
			}
		})
	}
}
