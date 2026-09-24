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
			releases[i], _ = reg.acquire(key, nil, func(ctx context.Context) {
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

	r1, _ := reg.acquire(tipProducerKey{asset: "native", quote: "fiat:USD", window: 5}, nil, start)
	r2, _ := reg.acquire(tipProducerKey{asset: "native", quote: "fiat:USD", window: 10}, nil, start)
	r3, _ := reg.acquire(tipProducerKey{asset: "crypto:BTC", quote: "fiat:USD", window: 5}, nil, start)
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

	rel1, _ := reg.acquire(key, nil, func(ctx context.Context) {
		<-ctx.Done()
		stopped.Store(true)
	})
	rel2, _ := reg.acquire(key, nil, func(context.Context) {
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

	rel, _ := reg.acquire(key, nil, start)
	rel()
	// Re-acquire inside the linger window: the pending stop must be
	// cancelled and the SAME producer keeps running.
	rel2, _ := reg.acquire(key, nil, start)
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
// A lingering producer yields its slot to a new mint, so the abort loop
// is admitted — but it can only ever replace lingering compute loops,
// never add to them. Refusal (and its count) is for a registry whose
// every slot is subscribed.
//
// Proven red against the pre-fix registry: acquire always succeeded, so
// running() reached the full attempt count.
func TestTipProducerRegistry_CeilingBoundsDetachedProducers(t *testing.T) {
	const ceiling = 8
	reg := &tipProducerRegistry{maxProducers: ceiling}
	var live atomic.Int32
	start := func(ctx context.Context) {
		live.Add(1)
		defer live.Add(-1)
		<-ctx.Done()
	}

	// Enumerate the window dimension of the key alone, exactly as the
	// cheapest attack would: one asset, one quote, many windows.
	for w := 1; w <= ceiling*4; w++ {
		key := tipProducerKey{asset: "native", quote: "fiat:USD", window: w}
		release, ok := reg.acquire(key, nil, start)
		if !ok {
			t.Fatalf("window %d refused while every slot was lingering", w)
		}
		// The attack shape: abort immediately. The producer survives via
		// linger, which is precisely why the connection cap cannot see it.
		release()
		if got := reg.running(); got > ceiling {
			t.Fatalf("running() = %d, exceeds ceiling %d — a detached producer "+
				"escaped the bound, which is the whole finding", got, ceiling)
		}
	}
	if !waitFor(time.Second, func() bool { return live.Load() <= ceiling }) {
		t.Errorf("%d compute loops still running, want at most the ceiling %d — "+
			"an evicted producer was deregistered but never stopped", live.Load(), ceiling)
	}

	// Every slot subscribed: now a new pair is refused, and counted.
	var held []func()
	t.Cleanup(func() {
		for _, rel := range held {
			rel()
		}
	})
	for w := 1; w <= ceiling*2; w++ {
		key := tipProducerKey{asset: "native", quote: "fiat:EUR", window: w}
		release, ok := reg.acquire(key, nil, start)
		if !ok {
			if release != nil {
				t.Fatal("refused acquire must not hand back a release func — " +
					"calling it would decrement a producer this caller never took")
			}
			continue
		}
		held = append(held, release)
	}
	if len(held) != ceiling {
		t.Errorf("admitted %d subscribed producers, want exactly the ceiling %d", len(held), ceiling)
	}
	if got := reg.refusedCount(); got != uint64(ceiling) {
		t.Errorf("refusedCount() = %d, want %d — refusals must be counted, "+
			"or a flood is survived silently instead of being visible", got, ceiling)
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
		release, ok := reg.acquire(key, nil, func(ctx context.Context) { <-ctx.Done() })
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
		nil,
		func(ctx context.Context) { <-ctx.Done() },
	); ok {
		t.Error("a new pair was admitted past the ceiling")
	}

	// ...while an existing one still joins.
	release, ok := reg.acquire(
		tipProducerKey{asset: "native", quote: "fiat:USD", window: 1},
		nil,
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
			nil,
			func(ctx context.Context) { <-ctx.Done() },
		); !ok {
			t.Fatalf("window %d refused with the ceiling disabled", w)
		}
	}
	if got := reg.running(); got != 64 {
		t.Errorf("running() = %d, want 64 with the ceiling disabled", got)
	}
}

// TestTipProducerRegistry_RespawnsAfterPanicWhileSubscriberStillConnected
// is the Q164 regression.
//
// worker.Recover stops a panicking compute loop's goroutine without
// releasing the registry entry: refs stays whatever it was, since
// release() is never called on a panic. Before this fix, nothing
// re-invoked start while refs > 0 — release()'s linger only fires once
// the LAST subscriber leaves, so an existing, still-connected subscriber
// (the one holding rel below) got heartbeats only, forever, for as long
// as it stayed connected: exactly the "no new emits" symptom the finding
// names.
func TestTipProducerRegistry_RespawnsAfterPanicWhileSubscriberStillConnected(t *testing.T) {
	reg := &tipProducerRegistry{restartBackoffFor: testRestartBackoff}
	var starts atomic.Int32
	key := tipProducerKey{asset: "native", quote: "fiat:USD", window: 5}

	rel, ok := reg.acquire(key, nil, func(ctx context.Context) {
		if starts.Add(1) == 1 {
			panic("boom") // recovered by worker.Recover inside spawn
		}
		<-ctx.Done()
	})
	if !ok {
		t.Fatal("acquire refused")
	}
	defer rel()

	if !waitFor(time.Second, func() bool { return starts.Load() >= 1 }) {
		t.Fatal("producer never started")
	}
	if !waitFor(3*time.Second, func() bool { return starts.Load() >= 2 }) {
		t.Fatalf("producer never respawned after a recovered panic (starts=%d) — "+
			"the still-connected subscriber holding rel would see no new emits "+
			"until every viewer of this pair disconnected", starts.Load())
	}
	if got := reg.running(); got != 1 {
		t.Fatalf("running() = %d after respawn, want 1 (same entry, not a duplicate)", got)
	}
}

// TestTipProducerRegistry_RespawnsAfterStartReturnsWhileSubscriberStillConnected
// is the production shape of the same defect: runSharedTipProducer
// recovers its own panic (recoverStreamProducer) and RETURNS normally, so
// the registry sees an uncancelled exit rather than a panic.
func TestTipProducerRegistry_RespawnsAfterStartReturnsWhileSubscriberStillConnected(t *testing.T) {
	reg := &tipProducerRegistry{restartBackoffFor: testRestartBackoff}
	var starts atomic.Int32
	key := tipProducerKey{asset: "native", quote: "fiat:USD", window: 7}

	rel, ok := reg.acquire(key, nil, func(ctx context.Context) {
		if starts.Add(1) == 1 {
			return // an internally-recovered panic looks exactly like this
		}
		<-ctx.Done()
	})
	if !ok {
		t.Fatal("acquire refused")
	}
	defer rel()

	if !waitFor(3*time.Second, func() bool { return starts.Load() >= 2 }) {
		t.Fatalf("producer never respawned after exiting uncancelled (starts=%d)", starts.Load())
	}
}

// testRestartBackoff stands in for tipProducerRestartBackoff so the
// respawn tests wait tens of milliseconds, not a wall-clock second.
const testRestartBackoff = 20 * time.Millisecond

// TestTipProducerRegistry_DoesNotRespawnAfterDeliberateStop pins the other
// side: a producer stopped by the linger must stay stopped.
func TestTipProducerRegistry_DoesNotRespawnAfterDeliberateStop(t *testing.T) {
	reg := &tipProducerRegistry{lingerFor: 10 * time.Millisecond, restartBackoffFor: testRestartBackoff}
	var starts atomic.Int32
	key := tipProducerKey{asset: "native", quote: "fiat:USD", window: 8}

	rel, ok := reg.acquire(key, nil, func(ctx context.Context) {
		starts.Add(1)
		<-ctx.Done()
	})
	if !ok {
		t.Fatal("acquire refused")
	}
	if !waitFor(time.Second, func() bool { return starts.Load() == 1 }) {
		t.Fatal("producer never started")
	}
	rel()
	if !waitFor(time.Second, func() bool { return reg.running() == 0 }) {
		t.Fatal("producer never stopped after the linger")
	}
	time.Sleep(10 * testRestartBackoff) // well past the backoff a respawn would wait
	if got := starts.Load(); got != 1 {
		t.Fatalf("starts = %d after a deliberate stop, want 1 (no respawn)", got)
	}
}

// TestStreamTimingDefaultsAreProduction pins the test-only timing
// overrides to their production values when unset, since the tests that
// use them bracket the shortened values rather than these.
func TestStreamTimingDefaultsAreProduction(t *testing.T) {
	s := New(Options{})
	if got := s.streamCadence(5); got != 5*time.Second {
		t.Errorf("streamCadence(5) = %v, want 5s", got)
	}
	if got := s.tipDivergenceBudget(); got != time.Second {
		t.Errorf("tipDivergenceBudget() = %v, want 1s", got)
	}
	if s.tipProducers.restartBackoffFor != 0 {
		t.Errorf("restartBackoffFor = %v, want 0 (tipProducerRestartBackoff)", s.tipProducers.restartBackoffFor)
	}
}
