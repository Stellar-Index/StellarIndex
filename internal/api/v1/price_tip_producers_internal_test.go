package v1

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Registry mechanics (RT-1): the whole point of the shared-producer
// shape is ONE compute loop per distinct pair regardless of viewer
// count, so these pin start-once, refcounted stop, linger absorption,
// and context cancellation — white-box, no server or DB involved.

func TestTipProducerRegistry_StartsOncePerKeyAcrossConcurrentAcquires(t *testing.T) {
	var reg tipProducerRegistry
	var starts atomic.Int32
	key := tipProducerKey{asset: "native", quote: "fiat:USD", window: 5}

	const viewers = 16
	releases := make([]func(), viewers)
	var wg sync.WaitGroup
	for i := 0; i < viewers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			releases[i] = reg.acquire(key, func(ctx context.Context) {
				starts.Add(1)
				<-ctx.Done()
			})
		}(i)
	}
	wg.Wait()

	if got := starts.Load(); got != 1 {
		t.Fatalf("start called %d times for one key, want exactly 1", got)
	}
	if got := reg.running(); got != 1 {
		t.Fatalf("running() = %d, want 1", got)
	}
	for _, rel := range releases {
		rel()
	}
}

func TestTipProducerRegistry_DistinctKeysGetDistinctProducers(t *testing.T) {
	var reg tipProducerRegistry
	var starts atomic.Int32
	start := func(ctx context.Context) { starts.Add(1); <-ctx.Done() }

	r1 := reg.acquire(tipProducerKey{asset: "native", quote: "fiat:USD", window: 5}, start)
	r2 := reg.acquire(tipProducerKey{asset: "native", quote: "fiat:USD", window: 10}, start)
	r3 := reg.acquire(tipProducerKey{asset: "crypto:BTC", quote: "fiat:USD", window: 5}, start)
	defer r1()
	defer r2()
	defer r3()

	if !waitFor(time.Second, func() bool { return starts.Load() == 3 }) {
		t.Fatalf("starts = %d, want 3", starts.Load())
	}
	if got := reg.running(); got != 3 {
		t.Fatalf("running() = %d, want 3", got)
	}
}

func TestTipProducerRegistry_LastReleaseStopsProducerAfterLinger(t *testing.T) {
	reg := tipProducerRegistry{lingerFor: 10 * time.Millisecond}
	var stopped atomic.Bool
	key := tipProducerKey{asset: "native", quote: "fiat:USD", window: 5}

	rel1 := reg.acquire(key, func(ctx context.Context) {
		<-ctx.Done()
		stopped.Store(true)
	})
	rel2 := reg.acquire(key, func(context.Context) {
		t.Error("second acquire must not start a new producer")
	})

	rel1()
	time.Sleep(30 * time.Millisecond)
	if stopped.Load() {
		t.Fatal("producer stopped while a reference was still held")
	}

	rel2()
	if !waitFor(time.Second, func() bool { return stopped.Load() }) {
		t.Fatal("producer never stopped after last release + linger")
	}
	if !waitFor(time.Second, func() bool { return reg.running() == 0 }) {
		t.Fatalf("running() = %d after stop, want 0", reg.running())
	}
}

func TestTipProducerRegistry_ReacquireDuringLingerKeepsProducer(t *testing.T) {
	reg := tipProducerRegistry{lingerFor: 25 * time.Millisecond}
	var starts, stops atomic.Int32
	key := tipProducerKey{asset: "native", quote: "fiat:USD", window: 5}
	start := func(ctx context.Context) {
		starts.Add(1)
		<-ctx.Done()
		stops.Add(1)
	}

	rel := reg.acquire(key, start)
	rel()
	// Re-acquire inside the linger window: the pending stop must be
	// cancelled and the SAME producer keeps running.
	rel2 := reg.acquire(key, start)
	time.Sleep(60 * time.Millisecond) // well past the original linger deadline
	if got := stops.Load(); got != 0 {
		t.Fatalf("producer stopped despite re-acquire during linger (stops=%d)", got)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("start called %d times, want 1 (reuse, not restart)", got)
	}
	rel2()
	if !waitFor(time.Second, func() bool { return stops.Load() == 1 }) {
		t.Fatalf("stops = %d after final release, want 1", stops.Load())
	}
}
