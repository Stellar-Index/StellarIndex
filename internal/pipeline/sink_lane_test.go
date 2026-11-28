package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func (f *fakeTradeStore) landedFrom(source string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.landed {
		if t.Source == source {
			n++
		}
	}
	return n
}

// With Postgres down, every on-chain worker holds its batch in the infra retry (that is what gates the cursor). The
// venue-stream lane must keep taking trades off its own channel meanwhile, and land them once Postgres is back: a
// venue stream nobody reads is dropped by the venue, and those trades have no cursor to replay from.
func TestExternalLaneDrainsWhileOnChainLaneRetries(t *testing.T) {
	store := &fakeTradeStore{} // unhealthy: every insert is an infra fault
	noEvents := func(context.Context, consumer.Event, bool) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	onChain := make(chan consumer.Event, 16)
	venues := make(chan consumer.Event, 16)

	var lanes sync.WaitGroup
	lanes.Add(2)
	go func() {
		defer lanes.Done()
		runSinkLane(ctx, discardLogger(), noEvents, store, onChain, SinkModeAll, PersistWorkers, newExternalRetryBuffer(store, discardLogger(), 0))
	}()
	go func() {
		defer lanes.Done()
		runSinkLane(ctx, discardLogger(), noEvents, boundedTradeWriter{store, externalWriteTimeout}, venues, SinkModeAll,
			ExternalPersistWorkers, newExternalRetryBuffer(store, discardLogger(), 0))
	}()

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for ledger := uint32(1); ; ledger++ {
			select {
			case onChain <- sdex.TradeEvent{Trade: mkTrade("sdex", ledger)}:
			case <-ctx.Done():
				return
			}
		}
	}()

	// More than the lane could hold without writing: its channel plus a full batch per worker.
	const venueTrades = 3 * (16 + ExternalPersistWorkers*tradeBatchSize)
	for i := 0; i < venueTrades; i++ {
		select {
		case venues <- external.TradeEvent{Trade: mkTrade("binance", 0)}:
		case <-time.After(5 * time.Second):
			t.Fatalf("the venue-stream lane stopped taking trades after %d while Postgres is down", i)
		}
	}
	// The on-chain lane is held throughout: nothing lands and, once every worker is in its retry, its channel stays full.
	waitFor(t, func() bool { return len(onChain) == cap(onChain) })
	if got := store.landedCount(); got != 0 {
		t.Fatalf("%d trades landed while the store is down", got)
	}

	store.healthy.Store(true)
	waitFor(t, func() bool { return store.landedFrom("binance") == venueTrades })

	cancel()
	<-pumpDone
	close(onChain)
	close(venues)
	lanes.Wait()
}

// Off-chain trades the retry buffer still holds when its final pass fails are abandoned with the lane, so the lane's
// ShutdownLoss must carry them and their refetch window; a zero loss reads as a clean shutdown.
func TestRunSinkLane_RetryBufferLossReachesShutdownLoss(t *testing.T) {
	store := &fakeTradeStore{} // stays unhealthy
	buf := newExternalRetryBuffer(store, discardLogger(), 0)
	buf.shutdownBudget = 50 * time.Millisecond
	for i := 0; i < 3; i++ {
		buf.enqueue(mkTrade("kraken", 0))
	}
	in := make(chan consumer.Event)
	close(in)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	loss := runSinkLane(ctx, discardLogger(), func(context.Context, consumer.Event, bool) error { return nil },
		boundedTradeWriter{store, externalWriteTimeout}, in, SinkModeAll, ExternalPersistWorkers, buf)

	want := mkTrade("kraken", 0).Timestamp
	if loss.Rows != 3 || !loss.LedgerUnknown || !loss.OffChainFrom.Equal(want) || !loss.OffChainTo.Equal(want) {
		t.Fatalf("loss = %+v; want 3 ledgerless rows over the window [%v, %v]", loss, want, want)
	}
}

// A worker still running after cancellation can hand the retry buffer a trade (an infra fault on a flush the racy
// select ran after the cancel). With the store still down at the end of the budget that trade must be named in the
// lane's loss, not left in a ring nothing drains.
func TestRunSinkLane_RetryBufferLateEnqueueIsALoss(t *testing.T) {
	store := &fakeTradeStore{} // stays unhealthy
	buf := newExternalRetryBuffer(store, discardLogger(), 0)
	buf.shutdownBudget = 50 * time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	ep := func(context.Context, consumer.Event, bool) error {
		close(entered)
		<-release
		return nil
	}
	in := make(chan consumer.Event, 1)
	in <- band.UpdateEvent{Update: canonical.OracleUpdate{Source: band.SourceName}}
	close(in)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan ShutdownLoss, 1)
	go func() {
		done <- runSinkLane(ctx, discardLogger(), ep, store, in, SinkModeAll, 1, buf)
	}()
	<-entered
	cancel()
	time.Sleep(100 * time.Millisecond) // long enough for a retrier tied to ctx to have run its final pass
	buf.enqueue(mkTrade("kraken", 0))  // what flushTradeBatch does with an infra-faulted external trade
	close(release)

	if loss := <-done; loss.Rows != 1 || !loss.LedgerUnknown {
		t.Fatalf("loss = %+v; want the trade buffered after the cancel counted (Rows 1, LedgerUnknown)", loss)
	}
}

// hangingTradeStore never answers: the write ends only when its ctx does.
type hangingTradeStore struct{ fakeTradeStore }

func (*hangingTradeStore) BatchInsertTrades(ctx context.Context, _ []canonical.Trade) error {
	<-ctx.Done()
	return ctx.Err()
}

// A Postgres that hangs must cost a venue-stream worker one bounded wait, and the result must read as an infra fault
// (buffer the trade). Left as a deadline error the sink would take it for shutdown and carry the batch forever.
func TestBoundedTradeWriterTurnsAHangIntoAnInfraFault(t *testing.T) {
	w := boundedTradeWriter{&hangingTradeStore{}, 20 * time.Millisecond}

	err := w.BatchInsertTrades(context.Background(), []canonical.Trade{mkTrade("binance", 0)})
	if !timescale.IsInfraError(err) || isCtxErr(err) {
		t.Fatalf("hung write = %v; want an infra fault that is not a ctx error", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.BatchInsertTrades(cancelled, nil); !isCtxErr(err) {
		t.Fatalf("write under a cancelled ctx = %v; want the ctx error, so shutdown still carries the batch", err)
	}
}
