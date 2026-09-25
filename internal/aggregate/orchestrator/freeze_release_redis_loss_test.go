package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── a release during a Redis loss must not end a sibling's freeze ───
//
// These tests build the writer the way cmd/stellarindex-aggregator does:
// WITH a ladder store. The window-isolation fixture builds
// freeze.NewWriter(rdb, 0) with none, so nothing it drives can reach the
// durable half of a release — which is where this defect lived. With the
// marker gone, the release of a recovering 5m window found "no sibling in
// the marker", cleared, and the clear retired the WHOLE durable record:
// the escalated 1h window, cold in this process, then rehydrated nothing
// and published.

// sinkShapedLadderStore mirrors timescale.FreezeEventSink's ladder
// semantics (the real sink is exercised in test/integration): a per-window
// map, a fail-closed pair-level summary, and a pair-level retire that drops
// every window's ladder with it.
type sinkShapedLadderStore struct {
	windows map[time.Duration]freeze.State
	retired bool
}

func newSinkShapedLadderStore() *sinkShapedLadderStore {
	return &sinkShapedLadderStore{windows: map[time.Duration]freeze.State{}}
}

func (s *sinkShapedLadderStore) summary() freeze.State {
	var out freeze.State
	for _, st := range s.windows {
		if !st.Active() {
			continue
		}
		if out.FiredAt.IsZero() || st.FiredAt.Before(out.FiredAt) {
			out.FiredAt = st.FiredAt
		}
		if st.HoldUntil.After(out.HoldUntil) {
			out.HoldUntil = st.HoldUntil
		}
		if st.ExtensionsUsed > out.ExtensionsUsed {
			out.ExtensionsUsed = st.ExtensionsUsed
		}
		out.Escalated = out.Escalated || st.Escalated
	}
	return out
}

func (s *sinkShapedLadderStore) SaveLadder(_ context.Context, _, _ canonical.Asset, st freeze.State) error {
	if !st.Active() { // NULL hold_until also NULLs window_ladders (SaveLadder's CASE)
		s.windows = map[time.Duration]freeze.State{}
		s.retired = true
	}
	return nil
}

func (s *sinkShapedLadderStore) LoadLadder(_ context.Context, _, _ canonical.Asset) (freeze.State, bool, error) {
	sum := s.summary()
	return sum, sum.Active(), nil
}

func (s *sinkShapedLadderStore) SaveWindowLadder(_ context.Context, _, _ canonical.Asset, w time.Duration, st freeze.State) error {
	if st.Active() {
		s.windows[w] = st
	} else {
		delete(s.windows, w)
	}
	return nil
}

func (s *sinkShapedLadderStore) LoadWindowLadders(_ context.Context, _, _ canonical.Asset) (map[time.Duration]freeze.State, freeze.State, bool, error) {
	if !s.summary().Active() {
		return nil, freeze.State{}, false, nil
	}
	out := make(map[time.Duration]freeze.State, len(s.windows))
	for k, v := range s.windows {
		out[k] = v
	}
	return out, freeze.State{}, true, nil
}

// productionWiredFixture is an orchestrator whose freeze writer carries a
// ladder store, as production's does, over a Redis that can be lost.
type productionWiredFixture struct {
	t           *testing.T
	flushRedis  func()
	store       *sinkShapedLadderStore
	pair        canonical.Pair
	short, long time.Duration
	newOrch     func() *Orchestrator
}

func newProductionWiredFixture(t *testing.T) *productionWiredFixture {
	t.Helper()
	rdb, mr := newTestRedis(t)
	f := &productionWiredFixture{
		t: t, flushRedis: mr.FlushAll, store: newSinkShapedLadderStore(),
		pair: xlmUSDPair(t), short: 5 * time.Minute, long: time.Hour,
	}
	f.newOrch = func() *Orchestrator {
		w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(f.store, 0))
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		o := New(&windowRoutedStore{byWindow: map[time.Duration][]canonical.Trade{}}, rdb, Config{
			Pairs: []canonical.Pair{f.pair}, Windows: []time.Duration{f.short, f.long}, Interval: time.Hour,
			FreezeWriter: w,
			Baselines: stubBaselineSource{multi: baseline.MultiBaseline{
				Day30: &baseline.Baseline{Median: 0, MAD: 0.001, N: maxDay30Returns},
			}},
		})
		if o.windowedFreeze == nil {
			t.Fatal("setup: the production writer must resolve as the windowed marker")
		}
		return o
	}
	return f
}

func (f *productionWiredFixture) key(w time.Duration) string {
	return f.pair.String() + ":" + w.String()
}

// freezeBoth marks the 1h window escalated and the 5m window one healthy
// tick from release, through the writer of `o`.
func (f *productionWiredFixture) freezeBoth(o *Orchestrator) (escalated, recovering freeze.State) {
	f.t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	escalated = freeze.State{
		FiredAt: now.Add(-2*time.Hour - 5*time.Minute), HoldUntil: now.Add(25 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions, Escalated: true,
	}
	recovering = freeze.State{FiredAt: now.Add(-15 * time.Minute), HoldUntil: now.Add(5 * time.Minute), UnfreezeStreak: 1}
	w, ok := o.cfg.FreezeWriter.(*freeze.Writer)
	if !ok {
		f.t.Fatalf("fixture FreezeWriter is %T, want the production *freeze.Writer", o.cfg.FreezeWriter)
	}
	if err := w.MarkHoldForWindow(ctx, f.pair.Base, f.pair.Quote, f.long, lkgFormatted,
		coldSiblingDecision(), escalated, 30*time.Minute); err != nil {
		f.t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	if err := w.MarkHoldForWindow(ctx, f.pair.Base, f.pair.Quote, f.short, lkgFormatted,
		coldSiblingDecision(), recovering, 10*time.Minute); err != nil {
		f.t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}
	return escalated, recovering
}

func healthySignal() freeze.Signal {
	return freeze.Signal{
		Now: time.Now().UTC(), Scored: true, Fires: false,
		Confidence: 0.99, ZScore: 0.1, ReleaseCorroborated: true,
	}
}

// TestFreezeLifecycle_ReleaseDuringRedisLossKeepsEscalatedSibling drives
// the real entry point. After a deploy the 5m window is live in memory and
// the 1h window — under the volume floor — has not reached the freeze step.
// Redis loses the marker, and the 5m window earns its release.
func TestFreezeLifecycle_ReleaseDuringRedisLossKeepsEscalatedSibling(t *testing.T) {
	f := newProductionWiredFixture(t)
	ctx := context.Background()
	_, recovering := f.freezeBoth(f.newOrch())

	o := f.newOrch() // the deploy: every key cold
	o.freezeStates[f.key(f.short)] = recovering
	f.flushRedis()

	refused := o.stepFreezeLifecycle(ctx, f.pair, f.short, f.key(f.short), healthySignal(),
		coldSiblingDecision(), big.NewRat(1, 8))
	if refused {
		t.Fatal("setup: the 5m window was expected to earn its release on this tick")
	}
	if f.store.retired {
		t.Error("the 5m window's release retired the WHOLE durable record during a Redis loss")
	}
	if got := f.store.windows[f.long]; !got.Escalated || got.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Errorf("1h durable ladder after the 5m release = %+v, want the escalated ladder intact", got)
	}
	if f.store.windows[f.short].Active() {
		t.Error("the released 5m window's durable ladder survived its release")
	}

	// The 1h window's next qualifying bucket: its first evaluation in this
	// process, against the only record left.
	st, overridden, err := o.loadFreezeState(ctx, f.pair, f.long, f.key(f.long))
	if err != nil {
		t.Fatalf("loadFreezeState: %v", err)
	}
	if overridden {
		t.Error("a Redis loss read as the operator override for the 1h window")
	}
	out := o.cfg.Phase2Thresholds.Lifecycle.Evaluate(st, healthySignal())
	if !st.Escalated || !out.Frozen {
		t.Errorf("1h window after its sibling's release: state=%+v Frozen=%v, want it escalated and "+
			"FROZEN — ADR-0019 holds an escalated freeze until manual unfreeze, and it published",
			st, out.Frozen)
	}
}

// TestFreezeLifecycle_OperatorOverrideStillSticksWithADurableRecord: asking
// the durable record before a clear must not make the override fail to
// take. `freeze-unfreeze` clears through the writer, which retires the
// durable record; every window then observes the absent marker, releases,
// and nothing re-freezes the pair.
func TestFreezeLifecycle_OperatorOverrideStillSticksWithADurableRecord(t *testing.T) {
	f := newProductionWiredFixture(t)
	ctx := context.Background()
	o := f.newOrch()
	escalated, recovering := f.freezeBoth(o)
	o.freezeStates[f.key(f.long)] = escalated
	o.freezeStates[f.key(f.short)] = recovering

	if err := o.cfg.FreezeWriter.Clear(ctx, f.pair.Base, f.pair.Quote); err != nil {
		t.Fatalf("operator Clear: %v", err)
	}

	for _, w := range []time.Duration{f.short, f.long} {
		if _, overridden, err := o.loadFreezeState(ctx, f.pair, w, f.key(w)); err != nil || !overridden {
			t.Fatalf("window %s did not observe the operator override", w)
		}
		o.releaseFreeze(ctx, f.pair, w, f.key(w), o.freezeStates[f.key(w)], freeze.TransitionOverridden)
		if o.freezeStates[f.key(w)].Active() {
			t.Errorf("window %s is still frozen in memory after the override", w)
		}
	}
	if len(f.store.windows) != 0 {
		t.Errorf("durable ladders after the override = %+v, want none", f.store.windows)
	}
	if st, present, _ := o.cfg.FreezeWriter.LoadState(ctx, f.pair.Base, f.pair.Quote); present {
		t.Errorf("the pair still reads as frozen after the override: %+v", st)
	}
}
