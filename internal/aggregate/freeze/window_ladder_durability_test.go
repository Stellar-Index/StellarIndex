package freeze_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── the durable ladder is per WINDOW (migration 0163) ───────────
//
// Migration 0119 gave the ADR-0019 ladder a durable home, but on the
// pair's single open `freeze_events` row: four columns, no window. The
// lifecycle runs one state machine per (pair, window), every frozen
// window mirrors its ladder on every tick, and so the durable record was
// whichever window wrote LAST. These tests pin the consequences that
// matter once Redis has lost the marker, which is the only time the
// durable ladder is read at all.

// windowedFakeLadderStore is fakeLadderStore plus the window-aware half.
//
// The pair-level SaveLadder / LoadLadder keep last-writer-wins semantics
// on purpose — that IS the pre-0163 store — so a Writer that still
// mirrors through them reproduces the defect, and one that uses the
// window-aware calls does not. The window-aware half maintains the same
// fail-closed pair-level summary the SQL does, because the recovery
// worker and `stellarindex-ops freeze-unfreeze -list` still read it.
type windowedFakeLadderStore struct {
	*fakeLadderStore
	windows map[string]map[time.Duration]freeze.State
	unowned map[string]freeze.State
}

func newWindowedFakeLadderStore() *windowedFakeLadderStore {
	return &windowedFakeLadderStore{
		fakeLadderStore: newFakeLadderStore(),
		windows:         map[string]map[time.Duration]freeze.State{},
		unowned:         map[string]freeze.State{},
	}
}

// SaveLadder is the pair-level write. An inactive state retires the whole
// durable record (what [freeze.Writer.Clear] relies on).
func (f *windowedFakeLadderStore) SaveLadder(ctx context.Context, asset, quote canonical.Asset, st freeze.State) error {
	if !st.Active() {
		delete(f.windows, f.key(asset, quote))
		delete(f.unowned, f.key(asset, quote))
	}
	return f.fakeLadderStore.SaveLadder(ctx, asset, quote, st)
}

func (f *windowedFakeLadderStore) SaveWindowLadder(
	_ context.Context, asset, quote canonical.Asset, window time.Duration, st freeze.State,
) error {
	k := f.key(asset, quote)
	if f.closed[k] {
		return nil
	}
	entries, ok := f.windows[k]
	if !ok {
		entries = map[time.Duration]freeze.State{}
		f.windows[k] = entries
		// First window-aware write onto a pre-0163 row: the pair-level
		// ladder has no owner, so it is kept as the unowned one.
		if legacy, had := f.states[k]; had && legacy.Active() {
			f.unowned[k] = legacy
		}
	}
	if st.Active() {
		entries[window] = st
	} else {
		delete(entries, window)
	}
	f.states[k] = f.summary(k)
	return nil
}

// summary is the fail-closed pair-level view: the furthest hold, the
// highest rung, escalated if ANY window is.
func (f *windowedFakeLadderStore) summary(k string) freeze.State {
	var out freeze.State
	fold := func(st freeze.State) {
		if !st.Active() {
			return
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
		out.Corroborated = out.Corroborated || st.Corroborated
	}
	for _, st := range f.windows[k] {
		fold(st)
	}
	fold(f.unowned[k])
	return out
}

func (f *windowedFakeLadderStore) LoadWindowLadders(
	_ context.Context, asset, quote canonical.Asset,
) (map[time.Duration]freeze.State, freeze.State, bool, error) {
	if f.err != nil {
		return nil, freeze.State{}, false, f.err
	}
	k := f.key(asset, quote)
	if f.closed[k] {
		return nil, freeze.State{}, false, nil
	}
	pair, ok := f.states[k]
	if !ok || pair.HoldUntil.IsZero() {
		return nil, freeze.State{}, false, nil
	}
	entries, windowed := f.windows[k]
	if !windowed {
		// A pre-0163 row: one pair-level ladder, owner unknown.
		return map[time.Duration]freeze.State{}, pair, true, nil
	}
	out := make(map[time.Duration]freeze.State, len(entries))
	for w, st := range entries {
		out[w] = st
	}
	return out, f.unowned[k], true, nil
}

func freshState(now time.Time) freeze.State {
	return freeze.State{
		FiredAt:   now.Add(-time.Minute),
		HoldUntil: now.Add(9 * time.Minute),
	}
}

// TestWriter_DurableLadderIsPerWindow is the regression for the durable
// half of the pair-keyed ladder.
//
// The 1h window has climbed the whole ladder and ESCALATED — ADR-0019
// holds it "until manual unfreeze". The 5m window of the same pair then
// fires a fresh freeze of its own. Both mirror their ladder durably on
// every tick. Redis is then lost and the aggregator restarts.
//
// Pre-fix the durable record was the LAST writer's: the 5m window's
// ten-minute, zero-extension, un-escalated ladder. Every window
// rehydrated that — so the escalated 1h freeze came back as an ordinary
// one that auto-unfreezes (the dangerous direction), and the 24h window,
// which was never frozen, came back frozen.
func TestWriter_DurableLadderIsPerWindow(t *testing.T) {
	mr, rdb := newRedis(t)
	store := newWindowedFakeLadderStore()
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(store, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	now := time.Now().UTC()

	escalated := escalatedState(now)
	fresh := freshState(now)
	if err := w.MarkHoldForWindow(ctx, asset, quote, longWindow, "0.1242",
		freezeDecision(), escalated, 30*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	if err := w.MarkHoldForWindow(ctx, asset, quote, shortWindow, "0.1242",
		freezeDecision(), fresh, 14*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}

	mr.FlushAll() // Redis is lost…
	restarted, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(store, 0))
	if err != nil { // …and the aggregator restarts.
		t.Fatalf("NewWriter (restart): %v", err)
	}

	gotLong, ok, err := restarted.LoadStateForWindow(ctx, asset, quote, longWindow)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(1h) = ok=%v err=%v, want present", ok, err)
	}
	if !gotLong.Escalated || gotLong.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Errorf("1h window rehydrated %+v, want its own ESCALATED ladder (extensions=%d): "+
			"the 5m window's later durable write replaced it, so an escalated freeze "+
			"resumes auto-unfreezing", gotLong, freeze.DefaultMaxExtensions)
	}

	gotShort, _, err := restarted.LoadStateForWindow(ctx, asset, quote, shortWindow)
	if err != nil {
		t.Fatalf("LoadStateForWindow(5m): %v", err)
	}
	if !gotShort.Active() || gotShort.Escalated || gotShort.ExtensionsUsed != 0 ||
		!gotShort.HoldUntil.Equal(fresh.HoldUntil) {
		t.Errorf("5m window rehydrated %+v, want its own fresh ladder %+v", gotShort, fresh)
	}

	gotDay, present, err := restarted.LoadStateForWindow(ctx, asset, quote, 24*time.Hour)
	if err != nil {
		t.Fatalf("LoadStateForWindow(24h): %v", err)
	}
	if gotDay.Active() {
		t.Errorf("24h window rehydrated %+v — it was never frozen; the pair-keyed durable "+
			"ladder copied a sibling's freeze onto it", gotDay)
	}
	if !present {
		t.Error("presence must stay pair-wide on the durable side too: the pair IS frozen, " +
			"so a window with no ladder of its own still reads present (a live freeze " +
			"must not read this as the operator override)")
	}
}

// TestWriter_FirstRemarkAfterRedisLossRestoresEveryWindow pins the write
// side of the same recovery. After a flush the first window to re-mark
// rebuilds the marker, and from then on the marker — not the durable
// store — answers every cold sibling. It must therefore be rebuilt with
// EVERY window's durable ladder, or the 1h window's escalation is lost
// one tick later than in the test above instead of not at all.
func TestWriter_FirstRemarkAfterRedisLossRestoresEveryWindow(t *testing.T) {
	mr, rdb := newRedis(t)
	store := newWindowedFakeLadderStore()
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(store, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	now := time.Now().UTC()

	escalated := escalatedState(now)
	fresh := freshState(now)
	for _, m := range []struct {
		window time.Duration
		state  freeze.State
	}{{longWindow, escalated}, {shortWindow, fresh}} {
		if err := w.MarkHoldForWindow(ctx, asset, quote, m.window, "0.1242",
			freezeDecision(), m.state, 30*time.Minute); err != nil {
			t.Fatalf("MarkHoldForWindow(%s): %v", m.window, err)
		}
	}
	mr.FlushAll()

	// The 5m window ticks first after the flush and re-marks.
	if err := w.MarkHoldForWindow(ctx, asset, quote, shortWindow, "0.1242",
		freezeDecision(), fresh, 14*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m) after flush: %v", err)
	}

	gotLong, ok, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(1h) = ok=%v err=%v, want present", ok, err)
	}
	if !gotLong.Escalated || !gotLong.FiredAt.Equal(escalated.FiredAt) {
		t.Errorf("1h window read %+v from the rebuilt marker, want its own escalated ladder %+v",
			gotLong, escalated)
	}
	gotDay, _, err := w.LoadStateForWindow(ctx, asset, quote, 24*time.Hour)
	if err != nil {
		t.Fatalf("LoadStateForWindow(24h): %v", err)
	}
	if gotDay.Active() {
		t.Errorf("24h window read %+v from the rebuilt marker — it was never frozen", gotDay)
	}
}

// TestRetireWindowLadder_RetiresTheDurableEntryToo: a window that
// auto-releases while a sibling stays frozen has its ladder dropped from
// the marker. The durable copy has to go with it, or a Redis loss before
// the sibling releases resurrects a freeze that already ended.
func TestRetireWindowLadder_RetiresTheDurableEntryToo(t *testing.T) {
	mr, rdb := newRedis(t)
	store := newWindowedFakeLadderStore()
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(store, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := w.MarkHoldForWindow(ctx, asset, quote, longWindow, "0.1242",
		freezeDecision(), escalatedState(now), 30*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	if err := w.MarkHoldForWindow(ctx, asset, quote, shortWindow, "0.1242",
		freezeDecision(), freshState(now), 14*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}
	if err := w.RetireWindowLadder(ctx, asset, quote, shortWindow); err != nil {
		t.Fatalf("RetireWindowLadder(5m): %v", err)
	}
	mr.FlushAll()

	gotShort, _, err := w.LoadStateForWindow(ctx, asset, quote, shortWindow)
	if err != nil {
		t.Fatalf("LoadStateForWindow(5m): %v", err)
	}
	if gotShort.Active() {
		t.Errorf("5m window rehydrated %+v after it had released — the durable record "+
			"kept a freeze the marker had already retired", gotShort)
	}
	gotLong, ok, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
	if err != nil || !ok || !gotLong.Escalated {
		t.Errorf("1h window = (%+v, ok=%v, err=%v), want its escalated ladder intact — "+
			"retiring a sibling must not touch it", gotLong, ok, err)
	}
}

// TestWriter_PreWindowDurableRowStillRehydratesEveryWindow is the
// fail-closed guard for a row written before migration 0163: it carries
// one pair-level ladder and nothing that says whose. Narrowing it to "no
// window owns this" would DROP a freeze that is still running, so it
// keeps answering for every window until window-aware writes replace it.
func TestWriter_PreWindowDurableRowStillRehydratesEveryWindow(t *testing.T) {
	_, rdb := newRedis(t)
	store := newWindowedFakeLadderStore()
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Written by the previous binary: pair-level only.
	if err := store.fakeLadderStore.SaveLadder(ctx, asset, quote, escalatedState(now)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(store, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, window := range []time.Duration{shortWindow, longWindow, 24 * time.Hour} {
		got, ok, err := w.LoadStateForWindow(ctx, asset, quote, window)
		if err != nil || !ok || !got.Escalated {
			t.Errorf("window %s = (%+v, ok=%v, err=%v), want the pair-level escalated ladder: "+
				"a pre-0163 row has no owner, and dropping it releases a live freeze",
				window, got, ok, err)
		}
	}
}
