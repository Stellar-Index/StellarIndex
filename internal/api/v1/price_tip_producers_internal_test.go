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
			releases[i], _ = reg.acquire(key, func(ctx context.Context) {
				starts.Add(1)
				<-ctx.Done()
			})
		}(i)
	}
	wg.Wait()

	// The start closure runs on its own goroutine — wait for it, then
	// assert no SECOND start ever fired.
	if !waitFor(time.Second, func() bool { return starts.Load() >= 1 }) {
		t.Fatal("producer start never ran")
	}
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

	r1, _ := reg.acquire(tipProducerKey{asset: "native", quote: "fiat:USD", window: 5}, start)
	r2, _ := reg.acquire(tipProducerKey{asset: "native", quote: "fiat:USD", window: 10}, start)
	r3, _ := reg.acquire(tipProducerKey{asset: "crypto:BTC", quote: "fiat:USD", window: 5}, start)
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

	rel1, _ := reg.acquire(key, func(ctx context.Context) {
		<-ctx.Done()
		stopped.Store(true)
	})
	rel2, _ := reg.acquire(key, func(context.Context) {
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

	rel, _ := reg.acquire(key, start)
	rel()
	// Re-acquire inside the linger window: the pending stop must be
	// cancelled and the SAME producer keeps running.
	rel2, _ := reg.acquire(key, start)
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

// TestTipProducerRegistry_CeilingBoundsDetachedProducers is the wave-D
// UNAUTH-DOS-1 regression.
//
// The SSE caps count CONNECTIONS, but a tip-stream connection also mints
// a DETACHED producer: context.Background(), outliving the request by
// design and surviving release for tipProducerLinger. So aborting the
// connection immediately does not stop the compute loop, and the
// connection cap never sees it.
//
// The producer key includes a CLIENT-CHOSEN window_seconds in [1,60], so
// the key space is pairs × 60 — an unauthenticated client can enumerate
// it without needing distinct assets. Without a ceiling, running() grows
// without bound and each entry polls the database on its own ticker.
//
// Proven red against the pre-fix registry: acquire always succeeded, so
// running() reached the full attempt count.
func TestTipProducerRegistry_CeilingBoundsDetachedProducers(t *testing.T) {
	const ceiling = 8
	reg := &tipProducerRegistry{maxProducers: ceiling}

	admitted := 0
	// Enumerate the window dimension of the key alone, exactly as the
	// cheapest attack would: one asset, one quote, many windows.
	for w := 1; w <= ceiling*4; w++ {
		key := tipProducerKey{asset: "native", quote: "fiat:USD", window: w}
		release, ok := reg.acquire(key, func(ctx context.Context) { <-ctx.Done() })
		if !ok {
			if release != nil {
				t.Fatal("refused acquire must not hand back a release func — " +
					"calling it would decrement a producer this caller never took")
			}
			continue
		}
		admitted++
		// The attack shape: abort immediately. The producer survives via
		// linger, which is precisely why the connection cap cannot see it.
		release()
	}

	if admitted != ceiling {
		t.Errorf("admitted %d producers, want exactly the ceiling %d", admitted, ceiling)
	}
	if got := reg.running(); got > ceiling {
		t.Errorf("running() = %d, exceeds ceiling %d — a detached producer "+
			"escaped the bound, which is the whole finding", got, ceiling)
	}
	if got := reg.refusedCount(); got != uint64(ceiling*4-ceiling) {
		t.Errorf("refusedCount() = %d, want %d — refusals must be counted, "+
			"or a flood is survived silently instead of being visible",
			got, ceiling*4-ceiling)
	}
}

// A pair that ALREADY has a producer must keep admitting subscribers at
// the ceiling: those cost nothing extra to serve, and refusing them
// would turn a popular pair's own viewers away — the opposite of the
// protection intended.
func TestTipProducerRegistry_CeilingStillAdmitsExistingPair(t *testing.T) {
	const ceiling = 4
	reg := &tipProducerRegistry{maxProducers: ceiling}

	var held []func()
	for w := 1; w <= ceiling; w++ {
		key := tipProducerKey{asset: "native", quote: "fiat:USD", window: w}
		release, ok := reg.acquire(key, func(ctx context.Context) { <-ctx.Done() })
		if !ok {
			t.Fatalf("window %d refused below the ceiling", w)
		}
		held = append(held, release)
	}
	t.Cleanup(func() {
		for _, rel := range held {
			rel()
		}
	})

	// A NEW key is refused...
	if _, ok := reg.acquire(
		tipProducerKey{asset: "native", quote: "fiat:EUR", window: 1},
		func(ctx context.Context) { <-ctx.Done() },
	); ok {
		t.Error("a new pair was admitted past the ceiling")
	}

	// ...while an existing one still joins.
	release, ok := reg.acquire(
		tipProducerKey{asset: "native", quote: "fiat:USD", window: 1},
		func(ctx context.Context) { <-ctx.Done() },
	)
	if !ok {
		t.Fatal("a second viewer of an ALREADY-RUNNING pair was refused at the " +
			"ceiling; that costs nothing extra to serve and refusing it " +
			"penalises the popular pair's own audience")
	}
	release()
}

// A negative ceiling is the operator's explicit "no limit" escape hatch.
func TestTipProducerRegistry_NegativeCeilingDisablesTheBound(t *testing.T) {
	reg := &tipProducerRegistry{maxProducers: -1}
	for w := 1; w <= 64; w++ {
		if _, ok := reg.acquire(
			tipProducerKey{asset: "native", quote: "fiat:USD", window: w},
			func(ctx context.Context) { <-ctx.Done() },
		); !ok {
			t.Fatalf("window %d refused with the ceiling disabled", w)
		}
	}
	if got := reg.running(); got != 64 {
		t.Errorf("running() = %d, want 64 with the ceiling disabled", got)
	}
}
