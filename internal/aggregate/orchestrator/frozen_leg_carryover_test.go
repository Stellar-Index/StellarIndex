package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestTriangulate_FrozenLegStaysRefusedOnATickItsWindowIsEmpty is the
// second half of MNY-22: a freeze must not be launderable through
// triangulation on the ticks AFTER the one that fired it either.
//
// The laundering guard read a set rebuilt at the top of every tick, and
// the only thing that ever wrote to it was engageFreeze. But
// refreshPairWindow returns BEFORE the freeze step when the window is
// empty, when it is under the USD-volume floor, and when the VWAP has no
// trades — and a pair whose market has just been manipulated on one thin
// venue is exactly the pair whose next bucket is empty. On that tick the
// pair is still frozen (its ADR-0019 hold runs for tens of minutes, its
// marker and its last-known-good value are both deliberately still in
// Redis) yet nothing re-entered it into the set, so the chain read the
// LKG as a fresh leg and published the product to a target that carries
// no frozen flag.
//
// Tick 1 here is the existing MNY-22 scenario. Tick 2 is the hole.
func TestTriangulate_FrozenLegStaysRefusedOnATickItsWindowIsEmpty(t *testing.T) {
	ctx := context.Background()
	leg1 := xlmUsdtPair(t) // the pair that freezes
	leg2 := mkPair(t, "crypto", "USDT", "fiat", "EUR")
	target := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute

	cache, mr := newTestRedis(t)
	o := New(nil, cache, Config{
		Pairs:        []canonical.Pair{leg1},
		Windows:      []time.Duration{window},
		Anomaly:      newAnomalyChecker(t, leg1),
		FreezeWriter: &recordingFreezeMarker{},
		Triangulations: []TriangulationChain{{
			Target: target,
			Legs:   []canonical.Pair{leg1, leg2},
		}},
	})

	leg1Key := cachekeys.VWAP(leg1.Base, leg1.Quote, window).String()
	leg2Key := cachekeys.VWAP(leg2.Base, leg2.Quote, window).String()
	targetKey := cachekeys.VWAP(target.Base, target.Quote, window).String()
	cache.Set(ctx, leg1Key, "1.000000000000", time.Minute)
	cache.Set(ctx, leg2Key, "0.900000000000", time.Hour)

	// Tick 1: prev = $1.00, this bucket prices XLM at ~$2.10 on one
	// source. The pair freezes and the chain is refused.
	stateKey := leg1.String() + ":" + window.String()
	o.prevVWAPs[stateKey] = big.NewRat(1, 1)
	o.store = &mockStore{trades: []canonical.Trade{
		buildTrade(t, big.NewInt(100_000_000), big.NewInt(210_000_000), time.Now()),
	}}
	if err := o.Tick(ctx); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	if !o.freezeStates[stateKey].Active() {
		t.Fatal("setup: the manipulated bucket did not freeze the leg")
	}
	if mr.Exists(targetKey) {
		t.Fatal("setup: tick 1 already published the derived price")
	}

	// Tick 2: the leg's window is EMPTY, so refreshPairWindow returns
	// before the freeze step. The freeze is 30 seconds old and its hold
	// has most of ten minutes left.
	o.store = &mockStore{}
	before := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcomeFrozenLeg))
	if err := o.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}

	if !o.freezeStates[stateKey].Active() {
		t.Fatal("the leg's freeze ended on a tick that never evaluated it")
	}
	if got, err := mr.Get(leg1Key); err != nil || got != "1.000000000000" {
		t.Fatalf("leg LKG = (%q, %v); the scenario needs it still in cache", got, err)
	}
	if mr.Exists(targetKey) {
		got, _ := mr.Get(targetKey)
		t.Errorf("target key %q written with %q on a tick the frozen leg's window was empty — "+
			"the leg's last-known-good was laundered into a derived price with no frozen flag",
			targetKey, got)
	}
	after := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcomeFrozenLeg))
	if after-before != 1 {
		t.Errorf("triangulation outcome %q delta on tick 2 = %v, want 1", outcomeFrozenLeg, after-before)
	}
}

// TestFrozenLeg_DoesNotOutliveTheHold bounds the carry-over. The
// in-memory ladder of a window that stops being evaluated is never
// advanced and never released, so reading it without a bound would refuse
// every chain through that pair for as long as its window stayed empty —
// long after the marker and the last-known-good value it protects have
// both expired out of Redis. The guard covers exactly the span the LKG
// can still be read back: the hold plus the marker grace.
func TestFrozenLeg_DoesNotOutliveTheHold(t *testing.T) {
	leg := xlmUsdtPair(t)
	window := 5 * time.Minute
	cache, _ := newTestRedis(t)
	o := New(nil, cache, Config{
		Pairs:   []canonical.Pair{leg},
		Windows: []time.Duration{window},
	})
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	o.clock = func() time.Time { return now }
	grace := o.cfg.Phase2Thresholds.Lifecycle.WithDefaults().MarkerGrace

	st := o.freezeStates[leg.String()+":"+window.String()]
	st.FiredAt = now.Add(-20 * time.Minute)
	st.HoldUntil = now.Add(-time.Minute) // hold over, inside the grace
	o.freezeStates[leg.String()+":"+window.String()] = st
	if !o.frozenLeg(leg, window) {
		t.Error("a leg inside its marker grace must still be refused: its LKG is still in cache")
	}

	now = st.HoldUntil.Add(grace + time.Second)
	if o.frozenLeg(leg, window) {
		t.Error("a leg whose hold and grace have both lapsed is still refused — its marker and " +
			"LKG are gone, and nothing will ever clear this in-memory ladder while the " +
			"window stays empty")
	}

	// A different window of the same pair is not implicated.
	now = st.HoldUntil.Add(-time.Minute)
	if o.frozenLeg(leg, time.Hour) {
		t.Error("the 1h window read as frozen on the 5m window's ladder")
	}
}
