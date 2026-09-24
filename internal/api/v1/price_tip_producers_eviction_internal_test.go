package v1

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A lingering producer (every subscriber gone, kept alive only to absorb
// a reconnect) must not refuse a real viewer's new pair. The global
// bounds evict the longest-idle lingering entries to make room, never a
// subscribed one, and evict nothing when eviction cannot make room.
//
// Proven red against the registry that counted lingering entries as
// demand: the bystander's mint below was refused with global_ceiling /
// global_rate_budget while every slot served nobody.

// stopTracker is a start func that records whether its producer was stopped.
type stopTracker struct{ stopped atomic.Bool }

func (s *stopTracker) start(ctx context.Context) {
	<-ctx.Done()
	s.stopped.Store(true)
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
