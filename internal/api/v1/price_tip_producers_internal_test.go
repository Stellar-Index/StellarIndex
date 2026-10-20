package v1

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Registry mechanics: the shared-producer shape means ONE compute loop per distinct pair regardless of viewer count.
// These white-box tests pin start-once, refcounted stop, linger absorption and context cancellation.

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

// TestTipProducerRegistry_CeilingBoundsDetachedProducers is the unauthenticated-DoS guard.
//
// The SSE caps count CONNECTIONS, but a tip-stream connection also mints a DETACHED producer
// (context.Background(), surviving release for tipProducerLinger). Aborting the connection does
// not stop the compute loop, and the connection cap never sees it.
//
// The producer key includes a CLIENT-CHOSEN window_seconds in [1,60], so the key space is
// pairs x 60 and an unauthenticated client can enumerate it without distinct assets. Without a
// ceiling, running() grows without bound and each entry polls the database on its own ticker.
//
// A lingering producer yields its slot to a new mint, so the abort loop is admitted but can only
// replace lingering compute loops, never add to them. Refusal (and its count) is for a registry
// whose every slot is subscribed. Red against a registry with no ceiling: acquire always
// succeeds, so running() reaches the full attempt count.
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

// A pair that already has a producer must keep admitting subscribers at the ceiling: they cost nothing extra, and
// refusing them would turn away a popular pair's own viewers.
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

// TestTipProducerRegistry_RespawnsAfterPanicWhileSubscriberStillConnected: worker.Recover stops a panicking compute loop
// without release(), so refs stays unchanged and nothing re-invokes start while refs > 0. Without the re-start, a
// still-connected subscriber would get heartbeats only, forever.
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

// TestTipProducerRegistry_RespawnsAfterStartReturnsWhileSubscriberStillConnected is the production shape of the same
// defect: runSharedTipProducer recovers its own panic and returns normally, an uncancelled exit.
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

func TestTipProducerRegistry_LingeringProducerYieldsItsSlotToANewViewer(t *testing.T) {
	const ceiling = 4
	reg := &tipProducerRegistry{maxProducers: ceiling, lingerFor: time.Hour}
	trackers := make([]*stopTracker, ceiling)
	keys := make([]tipProducerKey, ceiling)

	// Slot 0 is watched; slots 1..3 are the abort-loop shape — minted and
	// released at once, so they linger. Slot 1 has been idle longest.
	var held []func()
	t.Cleanup(func() {
		for _, rel := range held {
			rel()
		}
	})
	for i := range ceiling {
		trackers[i] = &stopTracker{}
		keys[i] = tipProducerKey{asset: "native", quote: "fiat:USD", window: i + 1}
		release, outcome := reg.acquireFor(keys[i], attackerCaller, nil, trackers[i].start)
		if outcome != tipProducerAdmitted {
			t.Fatalf("slot %d refused below the ceiling (%s)", i, outcome)
		}
		if i == 0 {
			held = append(held, release)
		} else {
			release()
		}
	}

	release, outcome := reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:EUR", window: 5},
		bystanderCaller, nil, func(ctx context.Context) { <-ctx.Done() })
	if outcome != tipProducerAdmitted {
		t.Fatalf("a bystander's new pair was refused (%s) while %d of %d slots "+
			"were lingering producers serving nobody", outcome, ceiling-1, ceiling)
	}
	held = append(held, release)

	if got := reg.running(); got != ceiling {
		t.Errorf("running() = %d, want the ceiling %d — eviction must replace, "+
			"not add", got, ceiling)
	}
	if !waitFor(time.Second, trackers[1].stopped.Load) {
		t.Error("the longest-idle lingering producer was not stopped on eviction")
	}
	for _, i := range []int{0, 2, 3} {
		if trackers[i].stopped.Load() {
			t.Errorf("producer %d was stopped; only the single longest-idle "+
				"lingering entry had to go", i)
		}
	}
	if got := reg.mintedFor(attackerCaller); got != ceiling-1 {
		t.Errorf("mintedFor(attacker) = %d, want %d — an evicted entry must "+
			"discharge its minter like a linger expiry does", got, ceiling-1)
	}

	// Once every slot is subscribed there is nothing to evict: refuse.
	for _, i := range []int{2, 3} {
		rel, o := reg.acquireFor(keys[i], bystanderCaller, nil, trackers[i].start)
		if o != tipProducerAdmitted {
			t.Fatalf("rejoining lingering producer %d refused (%s)", i, o)
		}
		held = append(held, rel)
	}
	if _, o := reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:GBP", window: 5},
		bystanderCaller, nil, func(ctx context.Context) { <-ctx.Done() },
	); o != tipProducerAtGlobalCeiling {
		t.Fatalf("mint with every slot subscribed = %s, want %s", o, tipProducerAtGlobalCeiling)
	}
	if trackers[0].stopped.Load() || trackers[2].stopped.Load() || trackers[3].stopped.Load() {
		t.Error("a subscribed producer was evicted")
	}
}

func TestTipProducerRegistry_LingeringProducerYieldsItsRateBudget(t *testing.T) {
	// Budget for exactly one 1s-window producer.
	reg := &tipProducerRegistry{maxTicks: tipTicksPerMinute(1), lingerFor: time.Hour}
	idle := &stopTracker{}
	release, outcome := reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:USD", window: 1},
		attackerCaller, nil, idle.start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("first mint refused (%s)", outcome)
	}
	release()

	release, outcome = reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:EUR", window: 1},
		bystanderCaller, nil, func(ctx context.Context) { <-ctx.Done() })
	if outcome != tipProducerAdmitted {
		t.Fatalf("a bystander's mint was refused (%s) while the whole rate budget "+
			"was held by a lingering producer serving nobody", outcome)
	}
	t.Cleanup(release)
	if !waitFor(time.Second, idle.stopped.Load) {
		t.Error("the lingering producer was not stopped on eviction")
	}
	if got := tipTestTicksPerMinute(reg, ""); got > tipTicksPerMinute(1) {
		t.Errorf("registered producers drive %d ticks/min, over the %d budget",
			got, tipTicksPerMinute(1))
	}

	if _, outcome = reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:GBP", window: 1},
		bystanderCaller, nil, func(ctx context.Context) { <-ctx.Done() },
	); outcome != tipProducerAtGlobalRateBudget {
		t.Fatalf("mint against a subscribed budget = %s, want %s",
			outcome, tipProducerAtGlobalRateBudget)
	}
}

// Eviction is all-or-nothing: if dropping every lingering entry still
// would not make room, none is dropped — the refused mint must not cost
// the reconnect cache anything.
func TestTipProducerRegistry_NoEvictionWhenItCannotMakeRoom(t *testing.T) {
	reg := &tipProducerRegistry{maxTicks: tipTicksPerMinute(1), lingerFor: time.Hour}
	idle := &stopTracker{}
	rel, outcome := reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:USD", window: 5},
		attackerCaller, nil, idle.start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("lingering mint refused (%s)", outcome)
	}
	rel()
	watched, outcome := reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:USD", window: 2},
		bystanderCaller, nil, func(ctx context.Context) { <-ctx.Done() })
	if outcome != tipProducerAdmitted {
		t.Fatalf("watched mint refused (%s)", outcome)
	}
	t.Cleanup(watched)

	// 30 ticks subscribed + 60 requested exceeds 60 even with the idle 12 gone.
	if _, outcome = reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:EUR", window: 1},
		bystanderCaller, nil, func(ctx context.Context) { <-ctx.Done() },
	); outcome != tipProducerAtGlobalRateBudget {
		t.Fatalf("outcome = %s, want %s", outcome, tipProducerAtGlobalRateBudget)
	}
	if got := reg.running(); got != 2 {
		t.Errorf("running() = %d, want 2 — a refused mint evicted a lingering entry", got)
	}
	time.Sleep(20 * time.Millisecond)
	if idle.stopped.Load() {
		t.Error("the lingering producer was stopped by a mint that was refused anyway")
	}
}

func TestTipProducerRegistry_RefusalsAreCountedByReason(t *testing.T) {
	blockForever := func(ctx context.Context) { <-ctx.Done() }
	quota := obs.APITipProducersRefusedTotal.WithLabelValues(tipProducerAtCallerQuota.String())
	ceiling := obs.APITipProducersRefusedTotal.WithLabelValues(tipProducerAtGlobalCeiling.String())
	quotaBefore, ceilingBefore := testutil.ToFloat64(quota), testutil.ToFloat64(ceiling)

	perCaller := &tipProducerRegistry{maxPerCaller: 1}
	rel, outcome := perCaller.acquireFor(tipProducerKey{asset: "native", quote: "fiat:USD", window: 11},
		"203.0.113.0", nil, blockForever)
	if outcome != tipProducerAdmitted {
		t.Fatalf("first mint = %v, want admitted", outcome)
	}
	defer rel()
	if _, outcome = perCaller.acquireFor(tipProducerKey{asset: "native", quote: "fiat:USD", window: 12},
		"203.0.113.0", nil, blockForever); outcome != tipProducerAtCallerQuota {
		t.Fatalf("second mint = %v, want caller_quota", outcome)
	}

	global := &tipProducerRegistry{maxProducers: 1}
	rel2, ok := global.acquire(tipProducerKey{asset: "native", quote: "fiat:USD", window: 13}, nil, blockForever)
	if !ok {
		t.Fatal("first acquire refused")
	}
	defer rel2()
	if _, outcome = global.acquireFor(tipProducerKey{asset: "native", quote: "fiat:USD", window: 14},
		"198.51.100.0", nil, blockForever); outcome != tipProducerAtGlobalCeiling {
		t.Fatalf("over-ceiling mint = %v, want global_ceiling", outcome)
	}

	if got := testutil.ToFloat64(quota) - quotaBefore; got != 1 {
		t.Errorf("caller_quota refusals counted = %v, want 1", got)
	}
	if got := testutil.ToFloat64(ceiling) - ceilingBefore; got != 1 {
		t.Errorf("global_ceiling refusals counted = %v, want 1", got)
	}
}

func TestTipProducerRegistry_GaugeTracksRegisteredProducers(t *testing.T) {
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_tip_producers"})
	reg := &tipProducerRegistry{lingerFor: 10 * time.Millisecond, gauge: gauge}
	key := tipProducerKey{asset: "native", quote: "fiat:USD", window: 15}

	rel, ok := reg.acquire(key, nil, func(ctx context.Context) { <-ctx.Done() })
	if !ok {
		t.Fatal("acquire refused")
	}
	rel2, _ := reg.acquire(key, nil, func(ctx context.Context) { <-ctx.Done() })
	if got := testutil.ToFloat64(gauge); got != 1 {
		t.Fatalf("gauge = %v with one shared producer, want 1", got)
	}
	rel()
	rel2()
	if !waitFor(time.Second, func() bool { return testutil.ToFloat64(gauge) == 0 }) {
		t.Fatalf("gauge = %v after the linger expired, want 0", testutil.ToFloat64(gauge))
	}
}

func TestTipProducerRegistry_OneCallerCannotMonopoliseTheGlobalPool(t *testing.T) {
	const (
		ceiling  = 64
		quota    = 8
		attempts = 40
	)
	// A long linger is the flood's own shape: the producer outlives the
	// aborted connection, so releasing does NOT give the slot back.
	reg := &tipProducerRegistry{
		maxProducers: ceiling,
		maxPerCaller: quota,
		lingerFor:    time.Hour,
	}

	admitted, quotaRefusals := 0, 0
	for w := 1; w <= attempts; w++ {
		key := tipProducerKey{asset: "native", quote: "fiat:USD", window: w}
		release, outcome := reg.acquireFor(key, attackerCaller, nil,
			func(ctx context.Context) { <-ctx.Done() })
		switch outcome {
		case tipProducerAdmitted:
			admitted++
			// The attack shape: abort immediately.
			release()
		case tipProducerAtCallerQuota:
			quotaRefusals++
			if release != nil {
				t.Fatal("a refused acquire must not hand back a release func — " +
					"calling it would discharge a producer this caller never took")
			}
		case tipProducerAtGlobalCeiling:
			t.Fatalf("window %d hit the GLOBAL ceiling at %d producers; one caller "+
				"reached the shared pool's bound, which is the finding", w, reg.running())
		case tipProducerAtCallerRateBudget, tipProducerAtGlobalRateBudget:
			t.Fatalf("window %d hit the untuned rate budget (%s); this test sizes only "+
				"the count quota and must not have enough windows/quota to reach it", w, outcome)
		}
	}

	if admitted != quota {
		t.Errorf("one caller minted %d producers, want exactly its quota %d", admitted, quota)
	}
	if got := reg.running(); got != quota {
		t.Errorf("running() = %d, want %d — every slot past the quota is a slot "+
			"this caller took from everyone else", got, quota)
	}
	if got := reg.mintedFor(attackerCaller); got != quota {
		t.Errorf("mintedFor(attacker) = %d, want %d — the charge must survive the "+
			"aborted connection for as long as the producer's entry does", got, quota)
	}
	if quotaRefusals != attempts-quota {
		t.Errorf("per-caller refusals = %d, want %d", quotaRefusals, attempts-quota)
	}
	if got := reg.refusedPerCallerCount(); got != uint64(attempts-quota) {
		t.Errorf("refusedPerCallerCount() = %d, want %d — a flood must be visible, "+
			"not merely survived", got, attempts-quota)
	}

	// The whole point: a bystander's unwatched pair is still served.
	release, outcome := reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:EUR", window: 1},
		bystanderCaller, nil, func(ctx context.Context) { <-ctx.Done() })
	if outcome != tipProducerAdmitted {
		t.Fatalf("a bystander's new pair was refused (%s) while one address held "+
			"%d producers — that 503 is the harm the quota exists to prevent",
			outcome, quota)
	}
	release()
}

// The charge is held for the registry ENTRY's life, not the connection's.
// Releasing while the producer lingers must NOT give the slot back: the
// linger is precisely the window the abort-loop flood runs in.
func TestTipProducerRegistry_CallerSlotReturnsOnlyWhenTheEntryLeaves(t *testing.T) {
	const quota = 2
	reg := &tipProducerRegistry{maxPerCaller: quota, lingerFor: 20 * time.Millisecond}
	start := func(ctx context.Context) { <-ctx.Done() }

	for w := 1; w <= quota; w++ {
		release, outcome := reg.acquireFor(
			tipProducerKey{asset: "native", quote: "fiat:USD", window: w},
			attackerCaller, nil, start)
		if outcome != tipProducerAdmitted {
			t.Fatalf("window %d refused below the quota (%s)", w, outcome)
		}
		release()
	}

	// Still lingering → still charged.
	if _, outcome := reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:USD", window: quota + 1},
		attackerCaller, nil, start,
	); outcome != tipProducerAtCallerQuota {
		t.Fatalf("outcome = %s while this caller's producers were still lingering; "+
			"want %s — releasing the connection must not return the slot",
			outcome, tipProducerAtCallerQuota)
	}

	// Once the entries actually leave, the slots come back.
	if !waitFor(2*time.Second, func() bool { return reg.mintedFor(attackerCaller) == 0 }) {
		t.Fatalf("mintedFor(attacker) = %d after the linger expired, want 0 — "+
			"the charge leaked and the caller is permanently locked out",
			reg.mintedFor(attackerCaller))
	}
	release, outcome := reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:USD", window: quota + 1},
		attackerCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("outcome = %s after the linger expired, want %s", outcome, tipProducerAdmitted)
	}
	release()
}

// Joining an ALREADY-RUNNING producer costs nothing to serve and must
// never be charged — it is the page-reload case the linger exists for,
// and charging it would turn a popular pair's own audience away.
func TestTipProducerRegistry_JoiningAnExistingProducerIsNeverCharged(t *testing.T) {
	reg := &tipProducerRegistry{maxPerCaller: 1, lingerFor: time.Hour}
	start := func(ctx context.Context) { <-ctx.Done() }
	key := tipProducerKey{asset: "native", quote: "fiat:USD", window: 5}

	first, outcome := reg.acquireFor(key, attackerCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("first acquire = %s, want %s", outcome, tipProducerAdmitted)
	}
	defer first()

	second, outcome := reg.acquireFor(key, attackerCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("the same caller's SECOND viewer of its own pair = %s, want %s",
			outcome, tipProducerAdmitted)
	}
	defer second()
	if got := reg.mintedFor(attackerCaller); got != 1 {
		t.Errorf("mintedFor(attacker) = %d after joining its own producer, want 1", got)
	}

	other, outcome := reg.acquireFor(key, bystanderCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("a bystander joining a running producer = %s, want %s",
			outcome, tipProducerAdmitted)
	}
	defer other()
	if got := reg.mintedFor(bystanderCaller); got != 0 {
		t.Errorf("mintedFor(bystander) = %d for a JOIN, want 0 — only mints are charged", got)
	}
}

// The charge follows the entry's holders, not whoever minted it: a caller who closed every stream must not stay charged
// or it is refused new pairs while holding none. The charge moves to a remaining holder; the linger after the last
// holder leaves stays charged to it, which is the window the abort-loop flood exploits.
func TestTipProducerRegistry_MinterLeavingHandsTheChargeToAHolder(t *testing.T) {
	reg := &tipProducerRegistry{maxPerCaller: 1, lingerFor: time.Hour}
	start := func(ctx context.Context) { <-ctx.Done() }
	key := tipProducerKey{asset: "native", quote: "fiat:USD", window: 5}
	ticks := tipTicksPerMinute(key.window)

	minted, outcome := reg.acquireFor(key, attackerCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("mint = %s, want %s", outcome, tipProducerAdmitted)
	}
	minterSecond, outcome := reg.acquireFor(key, attackerCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("minter's second viewer = %s, want %s", outcome, tipProducerAdmitted)
	}
	joined, outcome := reg.acquireFor(key, bystanderCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("join = %s, want %s", outcome, tipProducerAdmitted)
	}

	// The minter still holds one stream: it still pays.
	minted()
	if got := reg.mintedFor(attackerCaller); got != 1 {
		t.Fatalf("mintedFor(minter) = %d while it still holds a stream, want 1", got)
	}

	minterSecond()
	if got := reg.mintedFor(attackerCaller); got != 0 {
		t.Errorf("mintedFor(minter) = %d after its last stream closed while another "+
			"caller keeps the entry alive, want 0", got)
	}
	if got := reg.mintedTicks[attackerCaller]; got != 0 {
		t.Errorf("mintedTicks[minter] = %d after it left, want 0", got)
	}
	if got := reg.mintedFor(bystanderCaller); got != 1 {
		t.Errorf("mintedFor(holder) = %d, want 1 — the charge must move to the caller "+
			"keeping the entry alive, not vanish", got)
	}
	if got := reg.mintedTicks[bystanderCaller]; got != ticks {
		t.Errorf("mintedTicks[holder] = %d, want %d", got, ticks)
	}
	if reg.ticks != ticks {
		t.Errorf("aggregate ticks = %d after a transfer, want %d unchanged", reg.ticks, ticks)
	}
	if got := reg.running(); got != 1 {
		t.Fatalf("running() = %d, want 1 — the producer must keep serving its holder", got)
	}

	other, outcome := reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:USD", window: 6}, attackerCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("former minter's next pair = %s, want %s — it holds no streams", outcome,
			tipProducerAdmitted)
	}
	other()

	// The last holder leaving starts the linger, charged to that holder.
	joined()
	if got := reg.mintedFor(bystanderCaller); got != 1 {
		t.Errorf("mintedFor(last holder) = %d during the linger, want 1", got)
	}
}

// A caller joining a LINGERING entry — its minter already left and holds
// no reference — takes over the charge. Otherwise the minter stays
// charged for an entry kept alive only by someone else's stream.
func TestTipProducerRegistry_JoinDuringLingerTakesTheCharge(t *testing.T) {
	reg := &tipProducerRegistry{maxPerCaller: 1, lingerFor: time.Hour}
	start := func(ctx context.Context) { <-ctx.Done() }
	key := tipProducerKey{asset: "native", quote: "fiat:USD", window: 5}
	ticks := tipTicksPerMinute(key.window)

	minted, outcome := reg.acquireFor(key, attackerCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("mint = %s, want %s", outcome, tipProducerAdmitted)
	}
	minted() // linger armed; the minter stays charged through it
	if got := reg.mintedFor(attackerCaller); got != 1 {
		t.Fatalf("mintedFor(minter) = %d during the linger, want 1", got)
	}

	joined, outcome := reg.acquireFor(key, bystanderCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("join during linger = %s, want %s", outcome, tipProducerAdmitted)
	}
	defer joined()
	if got := reg.mintedFor(attackerCaller); got != 0 {
		t.Errorf("mintedFor(minter) = %d after a join during the linger, want 0", got)
	}
	if got := reg.mintedFor(bystanderCaller); got != 1 {
		t.Errorf("mintedFor(joiner) = %d, want 1 — the charge moves to the holder", got)
	}
	if reg.ticks != ticks {
		t.Errorf("aggregate ticks = %d after a transfer, want %d unchanged", reg.ticks, ticks)
	}

	other, outcome := reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:USD", window: 6}, attackerCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("former minter's next pair = %s, want %s — it holds no streams", outcome,
			tipProducerAdmitted)
	}
	other()
}

func TestTipProducerRegistry_AggregateQueryRateIsBoundedNotJustTheCount(t *testing.T) {
	budget := defaultMaxTipProducers * (60 / defaultTipWindowSeconds)
	reg := &tipProducerRegistry{lingerFor: time.Hour}

	sawRateRefusal := false
	var held []func()
	t.Cleanup(func() {
		for _, rel := range held {
			rel()
		}
	})
	// Many addresses (RFC 5737 TEST-NET-3), each minting and holding
	// 1s-window producers on distinct pairs — a lingering producer yields
	// its rate to a new mint, so only subscribed ones reach the budget.
	for c := 1; c <= 64; c++ {
		caller := "203.0.113." + strconv.Itoa(c)
		for j := 0; j < 30; j++ {
			key := tipProducerKey{
				asset: "asset-" + strconv.Itoa(c) + "-" + strconv.Itoa(j),
				quote: "fiat:USD", window: minTipWindowSeconds,
			}
			release, outcome := reg.acquireFor(key, caller, nil,
				func(ctx context.Context) { <-ctx.Done() })
			if outcome == tipProducerAdmitted {
				held = append(held, release)
				continue
			}
			if outcome.String() == "global_rate_budget" {
				sawRateRefusal = true
			}
		}
	}

	if got := tipTestTicksPerMinute(reg, ""); got > budget {
		t.Errorf("registered producers drive %d ticks/min against a %d budget — "+
			"the count ceiling alone lets 1s windows multiply the DB query rate",
			got, budget)
	}
	if !sawRateRefusal {
		t.Error("no acquire was refused with reason global_rate_budget; a flood " +
			"stopped by the rate budget must be told apart from the count ceiling")
	}
}

func TestTipProducerRegistry_OneCallerCannotBuyRateWithShortWindows(t *testing.T) {
	const attempts = 40
	budget := defaultMaxTipProducersPerCaller * (60 / defaultTipWindowSeconds)
	reg := &tipProducerRegistry{lingerFor: time.Hour}

	admitted, rateRefusals := 0, 0
	for j := 0; j < attempts; j++ {
		key := tipProducerKey{
			asset: "asset-" + strconv.Itoa(j), quote: "fiat:USD", window: minTipWindowSeconds,
		}
		release, outcome := reg.acquireFor(key, attackerCaller, nil,
			func(ctx context.Context) { <-ctx.Done() })
		switch {
		case outcome == tipProducerAdmitted:
			admitted++
			release()
		case outcome.String() == "caller_rate_budget":
			rateRefusals++
		}
	}

	if got := tipTestTicksPerMinute(reg, attackerCaller); got > budget {
		t.Errorf("one caller's producers drive %d ticks/min against its %d share — "+
			"short windows let it buy %dx the rate the per-caller quota was sized for",
			got, budget, got/budget)
	}
	if rateRefusals != attempts-admitted {
		t.Errorf("caller_rate_budget refusals = %d, want %d", rateRefusals, attempts-admitted)
	}
	if got := reg.refusedPerCallerCount(); got != uint64(attempts-admitted) {
		t.Errorf("refusedPerCallerCount() = %d, want %d — a per-caller rate refusal "+
			"is still a per-caller refusal", got, attempts-admitted)
	}

	// The rate share must not undercut the count quota at the default
	// window: a legitimate viewer keeps every producer it could mint before.
	for j := 0; j < defaultMaxTipProducersPerCaller; j++ {
		key := tipProducerKey{
			asset: "asset-" + strconv.Itoa(j), quote: "fiat:EUR", window: defaultTipWindowSeconds,
		}
		release, outcome := reg.acquireFor(key, bystanderCaller, nil,
			func(ctx context.Context) { <-ctx.Done() })
		if outcome != tipProducerAdmitted {
			t.Fatalf("default-window producer %d refused (%s) below the count quota %d",
				j, outcome, defaultMaxTipProducersPerCaller)
		}
		release()
	}
}

// stopTracker is a start func that records whether its producer was stopped.
type stopTracker struct{ stopped atomic.Bool }

func (s *stopTracker) start(ctx context.Context) {
	<-ctx.Done()
	s.stopped.Store(true)
}

// tipTestTicksPerMinute sums the compute rate of the registered producers
// minted by caller ("" = every producer), charging a producer on a w-second
// ticker ceil(60/w) ticks per minute.
func tipTestTicksPerMinute(reg *tipProducerRegistry, caller string) int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	total := 0
	for key, p := range reg.active {
		if caller != "" && p.minter != caller {
			continue
		}
		total += (60 + key.window - 1) / key.window
	}
	return total
}

// Per-caller producer quota: the global ceiling alone lets one address loop pairs x window_seconds, aborting each connection,
// and fill every slot with detached junk producers (surviving for tipProducerLinger) so other callers get 503. Connection caps
// cannot see it; without the quota check and mint charge in acquireFor the attacker is admitted every time.

// attackerCaller / bystanderCaller are documentation-range addresses
// (RFC 5737), not anything routable.
const (
	attackerCaller  = "203.0.113.7"
	bystanderCaller = "198.51.100.9"
)

// The quota is only as good as its key. IPv6 callers must aggregate to
// their /64: a quota keyed on the full /128 is bypassed by
// rotating the low bits of a prefix the caller already controls.
func TestTipProducerCaller_KeysOnTheRotatableBlock(t *testing.T) {
	callerFor := func(remoteAddr string) string {
		req := httptest.NewRequest(http.MethodGet, "/v1/price/tip/stream?asset=native", nil)
		req.RemoteAddr = remoteAddr
		return tipProducerCaller(req)
	}

	v6a := callerFor("[2001:db8:1:2::dead]:51234")
	v6b := callerFor("[2001:db8:1:2::beef]:51235")
	if v6a != v6b {
		t.Errorf("two addresses in one /64 keyed as %q and %q; an IPv6 caller "+
			"rotating within its own prefix would get a fresh quota per address",
			v6a, v6b)
	}
	if want := "2001:db8:1:2::"; v6a != want {
		t.Errorf("IPv6 caller key = %q, want the /64 prefix %q", v6a, want)
	}

	if got, want := callerFor("203.0.113.7:51234"), "203.0.113.7"; got != want {
		t.Errorf("IPv4 caller key = %q, want the exact address %q", got, want)
	}
	if got, want := callerFor("[2001:db8:1:3::1]:51234"), "2001:db8:1:3::"; got != want {
		t.Errorf("a DIFFERENT /64 keyed as %q, want %q — distinct subscribers must "+
			"not share one quota bucket", got, want)
	}

	// Fail-closed: an unresolvable caller shares one bucket rather than
	// being exempted, and NEVER resolves to the unattributed identity the
	// quota does not apply to.
	if got := callerFor(""); got != unknownTipCaller {
		t.Errorf("unresolvable caller key = %q, want %q", got, unknownTipCaller)
	}
	for _, addr := range []string{"", "203.0.113.7:51234", "[2001:db8::1]:1", "not-an-addr"} {
		if got := callerFor(addr); got == unattributedTipCaller {
			t.Errorf("RemoteAddr %q resolved to the unattributed caller, which the "+
				"quota does not apply to — the HTTP path must always be charged", addr)
		}
	}
}

type countingPriceReader struct {
	noPriceReader
	calls atomic.Int64
}

func (c *countingPriceReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (PriceSnapshot, []string, bool, error) {
	c.calls.Add(1)
	return c.noPriceReader.LatestPrice(ctx, a, q)
}

// A stream refused at the producer ceiling must cost no price reads.
func TestPriceTipStream_ProducerCeilingRefusalRunsNoComputeTip(t *testing.T) {
	prices := &countingPriceReader{}
	s := New(Options{
		Prices: prices,
		Hub:    streaming.NewHub(0),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	s.tipProducers.maxProducers = 1
	held, outcome := s.tipProducers.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:EUR", window: 5}, "other", nil,
		func(ctx context.Context) { <-ctx.Done() })
	if outcome != tipProducerAdmitted {
		t.Fatalf("setup: %s", outcome)
	}
	defer held()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/price/tip/stream?asset=native&quote=fiat:USD&window_seconds=1", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if n := prices.calls.Load(); n != 0 {
		t.Fatalf("computeTip ran %d price reads before the producer refusal", n)
	}
}

// blockingTickHistory holds every TradesInRange call until release is closed, recording peak calls in flight. A compute
// reads its pairs sequentially, so the peak equals concurrent computeTip calls.
type blockingTickHistory struct {
	HistoryReader // nil — only TradesInRange is exercised
	release       chan struct{}
	inFlight      atomic.Int32
	peak          atomic.Int32
}

func (h *blockingTickHistory) TradesInRange(ctx context.Context, pair canonical.Pair, _, to time.Time, _ int) ([]canonical.Trade, error) {
	n := h.inFlight.Add(1)
	defer h.inFlight.Add(-1)
	for {
		p := h.peak.Load()
		if n <= p || h.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case <-h.release:
		return []canonical.Trade{{
			Source:      "kraken",
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(1_000_0000)),
			QuoteAmount: canonical.NewAmount(big.NewInt(200_0000)),
			Timestamp:   to.Add(-time.Second),
		}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestTipTicks_ConcurrencyIsBoundedOnTheRealTickPath drives both producer shapes' real tick loops with more producers
// than tick slots against a never-answering reader and requires in-flight computes never exceed slots. Count and rate
// budgets do not bound this: every producer can fire at once.
func TestTipTicks_ConcurrencyIsBoundedOnTheRealTickPath(t *testing.T) {
	const slots, producers = 2, 6
	btc, _ := canonical.ParseAsset("crypto:BTC")
	usd, _ := canonical.ParseAsset("fiat:USD")

	shapes := map[string]func(ctx context.Context, s *Server, i int){
		"shared": func(ctx context.Context, s *Server, i int) {
			key := tipProducerKey{asset: btc.String(), quote: usd.String(), window: 60 - i}
			s.runSharedTipProducer(ctx, key, btc, usd, 60-i)
		},
		"per_connection": func(ctx context.Context, s *Server, _ int) {
			var gen streaming.Generator
			ch := make(chan streaming.Event, 4)
			s.runTipStreamProducer(ctx, ch, &gen, btc, usd, 1, streaming.Event{})
		},
	}
	for name, run := range shapes {
		t.Run(name, func(t *testing.T) {
			h := &blockingTickHistory{release: make(chan struct{})}
			s := New(Options{
				History: h,
				Prices:  noPriceReader{},
				Hub:     streaming.NewHub(0),
				Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			s.tipProducers.tickConcurrency = slots

			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			for i := range producers {
				wg.Add(1)
				go func() { defer wg.Done(); run(ctx, s, i) }()
			}
			t.Cleanup(func() { cancel(); wg.Wait() })

			deadline := time.Now().Add(5 * time.Second)
			for h.inFlight.Load() < slots && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if got := h.inFlight.Load(); got < slots {
				t.Fatalf("only %d computes started; the tick path never reached the reader", got)
			}
			// Every producer has fired by now; give the unbounded shape the
			// time to pile past the slots.
			time.Sleep(300 * time.Millisecond)
			close(h.release)
			if peak := h.peak.Load(); peak > slots {
				t.Fatalf("peak concurrent tick computes = %d across %d producers, want <= %d (tick slots)",
					peak, producers, slots)
			}
		})
	}
}

// panicOnceHistory panics on its first TradesInRange call and answers
// with no trades after.
type panicOnceHistory struct {
	HistoryReader
	calls atomic.Int32
}

func (h *panicOnceHistory) TradesInRange(context.Context, canonical.Pair, time.Time, time.Time, int) ([]canonical.Trade, error) {
	if h.calls.Add(1) == 1 {
		panic("tip tick compute panicked")
	}
	return nil, nil
}

// TestTipTicks_SlotIsFreedWhenTheComputePanics pins that a tick slot is
// released when the compute panics and the producer's recover absorbs
// it. With one slot, a leaked slot would starve every later tick.
func TestTipTicks_SlotIsFreedWhenTheComputePanics(t *testing.T) {
	btc, _ := canonical.ParseAsset("crypto:BTC")
	usd, _ := canonical.ParseAsset("fiat:USD")
	h := &panicOnceHistory{}
	s := New(Options{
		History: h,
		Prices:  noPriceReader{},
		Hub:     streaming.NewHub(0),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	s.tipProducers.tickConcurrency = 1

	key := tipProducerKey{asset: btc.String(), quote: usd.String(), window: 60}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runSharedTipProducer(context.Background(), key, btc, usd, 60)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("producer did not exit on its panicking compute")
	}
	if got := h.calls.Load(); got != 1 {
		t.Fatalf("TradesInRange calls = %d, want the 1 that panicked", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := s.computeTipGated(ctx, btc, usd, 5)
	if ctx.Err() != nil {
		t.Fatalf("next tick could not get the only slot (err = %v): the panicked compute leaked it", err)
	}
}

// noPriceReader answers every read with ErrPriceNotFound.
type noPriceReader struct{}

func (noPriceReader) LatestPrice(context.Context, canonical.Asset, canonical.Asset) (PriceSnapshot, []string, bool, error) {
	return PriceSnapshot{}, nil, false, ErrPriceNotFound
}

func (noPriceReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]PriceSnapshot, error) {
	return nil, nil
}
