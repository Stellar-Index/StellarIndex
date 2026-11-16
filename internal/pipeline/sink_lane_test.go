package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
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
// off-chain lane must keep taking venue trades off its own channel meanwhile, and land them once Postgres is back:
// a venue stream nobody reads is dropped by the venue, and those trades have no cursor to replay from.
func TestExternalLaneDrainsWhileOnChainLaneRetries(t *testing.T) {
	store := &fakeTradeStore{} // unhealthy: every insert is an infra fault
	noEvents := func(context.Context, consumer.Event, bool) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	onChain := make(chan consumer.Event, 16)
	offChain := make(chan consumer.Event, 16)

	var lanes sync.WaitGroup
	lanes.Add(2)
	go func() {
		defer lanes.Done()
		runSinkLane(ctx, discardLogger(), noEvents, store, onChain, SinkModeAll, PersistWorkers, true)
	}()
	go func() {
		defer lanes.Done()
		runSinkLane(ctx, discardLogger(), offChainEventPersister(discardLogger(), noEvents), store, offChain, SinkModeAll, ExternalPersistWorkers, true)
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
	// Every on-chain worker is in its retry once the channel stays full.
	waitFor(t, func() bool { return len(onChain) == cap(onChain) })

	// More than the lane could hold without writing: its channel plus a full batch per worker.
	const venueTrades = 3 * (16 + ExternalPersistWorkers*tradeBatchSize)
	for i := 0; i < venueTrades; i++ {
		select {
		case offChain <- external.TradeEvent{Trade: mkTrade("binance", 0)}:
		case <-time.After(5 * time.Second):
			t.Fatalf("the off-chain lane stopped taking venue trades after %d while Postgres is down", i)
		}
	}
	if got := store.landedCount(); got != 0 {
		t.Fatalf("%d trades landed while the store is down", got)
	}
	if len(onChain) != cap(onChain) {
		t.Fatal("the on-chain lane is no longer backed up: the test did not hold it in its retry")
	}

	store.healthy.Store(true)
	waitFor(t, func() bool { return store.landedFrom("binance") == venueTrades })

	cancel()
	<-pumpDone
	close(onChain)
	close(offChain)
	lanes.Wait()
}

// A poll result that cannot be written is skipped, not block-retried: a worker held in retry is a worker not reading
// the venue trades. A permanent fault still reaches the caller's drop accounting.
func TestOffChainEventPersisterSkipsAnInfraFault(t *testing.T) {
	ev := external.UpdateEvent{Update: canonical.OracleUpdate{Source: "coingecko"}}
	calls := 0
	ep := offChainEventPersister(discardLogger(), func(context.Context, consumer.Event, bool) error {
		calls++
		return errInfra
	})
	before := counter(t, obs.SourceInsertErrorsTotal, "coingecko", "dropped")

	done := make(chan error, 1)
	go func() { done <- persistEventResilient(context.Background(), discardLogger(), ep, ev) }()
	select {
	case err := <-done:
		if err != nil || calls != 1 {
			t.Fatalf("err = %v after %d attempts; want nil after one", err, calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an off-chain event block-retried an infra fault")
	}
	if got := counter(t, obs.SourceInsertErrorsTotal, "coingecko", "dropped") - before; got != 1 {
		t.Errorf("dropped counter moved by %v, want 1", got)
	}

	permanent := offChainEventPersister(discardLogger(), func(context.Context, consumer.Event, bool) error { return errData })
	if err := permanent(context.Background(), ev, true); err == nil {
		t.Error("a permanent data fault was swallowed")
	}
}
