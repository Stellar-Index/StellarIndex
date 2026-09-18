package freeze_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// ─── E1: the marker carries one ladder PER window ─────────────────
//
// The `freeze:<asset>:<quote>` marker is keyed per pair — its presence
// is the pair-wide `flags.frozen` the API serves — while the ADR-0019
// lifecycle inside it advances per (pair, window). These tests pin the
// two halves that follow: a write records only the writing window's
// ladder (and preserves every other window's), and a read answers with
// the asking window's ladder and no one else's.

const (
	shortWindow = 5 * time.Minute
	longWindow  = time.Hour
)

func ladderDecision() anomaly.Decision {
	return anomaly.Decision{
		Action:       anomaly.ActionFreeze,
		Class:        anomaly.ClassCrypto,
		DeviationPct: 61.0,
		Reason:       "phase2:3_signal_AND",
	}
}

func ladderState(firedAt time.Time, extensions int) freeze.State {
	return freeze.State{
		FiredAt:        firedAt,
		HoldUntil:      firedAt.Add(30 * time.Minute),
		ExtensionsUsed: extensions,
	}
}

// TestMarkHoldForWindow_KeepsEachWindowsLadderApart is E1 at the
// storage layer: two windows of one pair freeze independently, and
// each must read back ITS OWN ladder — not the other's, and not the
// last one written.
func TestMarkHoldForWindow_KeepsEachWindowsLadderApart(t *testing.T) {
	_, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, time.Minute)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()

	fired := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	shortState := ladderState(fired, 0)
	longState := ladderState(fired.Add(-time.Hour), 3)

	if err := w.MarkHoldForWindow(ctx, asset, quote, shortWindow, "0.124200000000",
		ladderDecision(), shortState, time.Hour); err != nil {
		t.Fatalf("MarkHoldForWindow(short): %v", err)
	}
	if err := w.MarkHoldForWindow(ctx, asset, quote, longWindow, "0.124200000000",
		ladderDecision(), longState, time.Hour); err != nil {
		t.Fatalf("MarkHoldForWindow(long): %v", err)
	}

	gotShort, ok, err := w.LoadStateForWindow(ctx, asset, quote, shortWindow)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(short): ok=%v err=%v", ok, err)
	}
	if gotShort.ExtensionsUsed != 0 || !gotShort.FiredAt.Equal(shortState.FiredAt) {
		t.Errorf("short window read back %+v, want its own %+v — the later write for "+
			"the 1h window overwrote it", gotShort, shortState)
	}
	gotLong, ok, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(long): ok=%v err=%v", ok, err)
	}
	if gotLong.ExtensionsUsed != 3 || !gotLong.FiredAt.Equal(longState.FiredAt) {
		t.Errorf("long window read back %+v, want its own %+v", gotLong, longState)
	}

	// A window that never froze owns NO ladder — while the pair still
	// reads as frozen, because the marker is there.
	third, ok, err := w.LoadStateForWindow(ctx, asset, quote, 24*time.Hour)
	if err != nil {
		t.Fatalf("LoadStateForWindow(24h): %v", err)
	}
	if !ok {
		t.Error("presence must stay pair-wide: the marker exists, so every window of " +
			"the pair reads present (that is what flags.frozen and the operator " +
			"force-unfreeze are built on)")
	}
	if third.Active() {
		t.Errorf("the 24h window inherited a ladder it never earned: %+v", third)
	}
}

// TestLoadStateForWindow_LegacyMarkerAnswersPairWide pins the upgrade
// shape: a marker written before per-window ladders existed carries one
// pair-level state and no way to tell whose it is. It must keep
// answering for every window — dropping it instead would release a
// freeze that is still running, and on a restarted process (no
// prev-VWAP comparator) the window could not even re-fire on its own
// signal.
func TestLoadStateForWindow_LegacyMarkerAnswersPairWide(t *testing.T) {
	_, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, time.Minute)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()

	fired := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	legacy := freeze.Marker{
		AssetID:  asset.String(),
		QuoteID:  quote.String(),
		Action:   anomaly.ActionFreeze,
		FrozenAt: fired,
		State:    ladderState(fired, 2),
	}
	body, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy marker: %v", err)
	}
	key := cachekeys.Freeze(asset, quote).String()
	if err := rdb.Set(ctx, key, body, time.Hour).Err(); err != nil {
		t.Fatalf("seed legacy marker: %v", err)
	}

	got, ok, err := w.LoadStateForWindow(ctx, asset, quote, shortWindow)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow: ok=%v err=%v", ok, err)
	}
	if got.ExtensionsUsed != 2 || !got.FiredAt.Equal(fired) {
		t.Errorf("legacy marker read back %+v, want the pair-level ladder %+v",
			got, legacy.State)
	}

	// The owning window's next lifecycle write upgrades the marker in
	// place, and from then on the ladders are scoped.
	if err := w.MarkHoldForWindow(ctx, asset, quote, longWindow, "0.124200000000",
		ladderDecision(), ladderState(fired, 2), time.Hour); err != nil {
		t.Fatalf("MarkHoldForWindow: %v", err)
	}
	upgraded, ok, err := w.LoadStateForWindow(ctx, asset, quote, shortWindow)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow after upgrade: ok=%v err=%v", ok, err)
	}
	if upgraded.Active() {
		t.Errorf("after the owning window claimed its ladder, the 5m window still "+
			"reads %+v — it owns none", upgraded)
	}
}

// TestMarkHoldForWindow_MarkerLossKeepsEveryWindowFrozen pins the
// recovery that per-window ladders could otherwise narrow away.
//
// Redis loses the marker while both windows of a pair are frozen. The
// migration-0119 durable ladder is keyed (asset, quote) with no window
// column, so it says "this pair is inside an unreleased freeze" and
// nothing about whose. The first window to re-mark must NOT rewrite
// that pair-wide fact into "only I am frozen": the sibling is cold in
// the same recovery and, on a restarted process, has no prev-VWAP
// comparator to re-fire on, so reading the narrowed marker would make
// it publish the bucket the freeze was withholding.
func TestMarkHoldForWindow_MarkerLossKeepsEveryWindowFrozen(t *testing.T) {
	mr, rdb := newRedis(t)
	ladder := newFakeLadderStore()
	w, err := freeze.NewWriter(rdb, time.Minute, freeze.WithLadderStore(ladder, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()

	// Both windows frozen, then Redis loses the marker. The durable
	// ladder — the one authority left — is still live.
	fired := time.Now().Add(-10 * time.Minute)
	live := freeze.State{FiredAt: fired, HoldUntil: time.Now().Add(20 * time.Minute)}
	for _, window := range []time.Duration{shortWindow, longWindow} {
		if err := w.MarkHoldForWindow(ctx, asset, quote, window, "0.124200000000",
			ladderDecision(), live, time.Hour); err != nil {
			t.Fatalf("MarkHoldForWindow(%v): %v", window, err)
		}
	}
	mr.Del(cachekeys.Freeze(asset, quote).String())

	// Recovery: the 5m window rehydrates from the durable ladder and
	// re-marks first.
	if err := w.MarkHoldForWindow(ctx, asset, quote, shortWindow, "0.124200000000",
		ladderDecision(), live, time.Hour); err != nil {
		t.Fatalf("MarkHoldForWindow(short) after marker loss: %v", err)
	}

	got, ok, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(long): ok=%v err=%v", ok, err)
	}
	if !got.Active() {
		t.Error("the 1h window read no ladder after the 5m window re-marked: the " +
			"pair-wide durable record was narrowed to one window, so a window that " +
			"is still frozen would publish its next bucket")
	}
	if !got.HoldUntil.Equal(live.HoldUntil) {
		t.Errorf("the 1h window read %+v, want the pair's live ladder %+v", got, live)
	}

	// The 1h window claims it, and the 5m window's own ladder is
	// untouched by that.
	if err := w.MarkHoldForWindow(ctx, asset, quote, longWindow, "0.124200000000",
		ladderDecision(), live, time.Hour); err != nil {
		t.Fatalf("MarkHoldForWindow(long): %v", err)
	}
	if gotShort, _, err := w.LoadStateForWindow(ctx, asset, quote, shortWindow); err != nil {
		t.Fatalf("LoadStateForWindow(short): %v", err)
	} else if !gotShort.HoldUntil.Equal(live.HoldUntil) {
		t.Errorf("the 5m window's ladder read back %+v, want %+v", gotShort, live)
	}

	// A window that was NOT part of the recovery inherits nothing once
	// the unowned snapshot has outlived its own hold plus the grace: it
	// describes no running freeze, so nobody may adopt it.
	stale := freeze.State{
		FiredAt:   time.Now().Add(-3 * time.Hour),
		HoldUntil: time.Now().Add(-2 * time.Hour),
	}
	mr.Del(cachekeys.Freeze(asset, quote).String())
	ladder.states[asset.String()+"|"+quote.String()] = stale
	if err := w.MarkHoldForWindow(ctx, asset, quote, shortWindow, "0.124200000000",
		ladderDecision(), live, time.Hour); err != nil {
		t.Fatalf("MarkHoldForWindow(short) with a stale durable ladder: %v", err)
	}
	third, ok, err := w.LoadStateForWindow(ctx, asset, quote, 24*time.Hour)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(24h): ok=%v err=%v", ok, err)
	}
	if third.Active() {
		t.Errorf("the 24h window adopted a ladder whose hold lapsed hours ago: %+v", third)
	}
}

// TestRetireWindowLadder_LeavesTheMarkerAndTheSibling pins the release
// half: one window's ladder goes, the marker, its TTL and every other
// window's ladder stay.
func TestRetireWindowLadder_LeavesTheMarkerAndTheSibling(t *testing.T) {
	mr, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, time.Minute)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	key := cachekeys.Freeze(asset, quote).String()

	fired := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		window time.Duration
		state  freeze.State
	}{
		{shortWindow, ladderState(fired, 0)},
		{longWindow, ladderState(fired, 3)},
	} {
		if err := w.MarkHoldForWindow(ctx, asset, quote, tc.window, "0.124200000000",
			ladderDecision(), tc.state, time.Hour); err != nil {
			t.Fatalf("MarkHoldForWindow(%v): %v", tc.window, err)
		}
	}
	ttlBefore := mr.TTL(key)

	if err := w.RetireWindowLadder(ctx, asset, quote, shortWindow); err != nil {
		t.Fatalf("RetireWindowLadder: %v", err)
	}

	if !mr.Exists(key) {
		t.Fatal("retiring one window's ladder deleted the marker — the pair's " +
			"flags.frozen would go false while the 1h window is still frozen")
	}
	if got := mr.TTL(key); got != ttlBefore {
		t.Errorf("marker TTL = %v, want the sibling's remaining hold %v left untouched",
			got, ttlBefore)
	}
	gotShort, ok, err := w.LoadStateForWindow(ctx, asset, quote, shortWindow)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(short): ok=%v err=%v", ok, err)
	}
	if gotShort.Active() {
		t.Errorf("the released window still reads %+v — a restart would rehydrate a "+
			"freeze that had already ended", gotShort)
	}
	gotLong, ok, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(long): ok=%v err=%v", ok, err)
	}
	if gotLong.ExtensionsUsed != 3 {
		t.Errorf("the still-frozen window's ladder was collateral damage: %+v", gotLong)
	}
}
