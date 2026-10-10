package freeze_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

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

	// Relative to now: a ladder past its hold plus the grace is no
	// longer one the marker carries.
	fired := time.Now().UTC()
	shortState := ladderState(fired, 0)
	longState := ladderState(fired.Add(-10*time.Minute), 3)

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
	now := time.Now().UTC()
	w, err := freeze.NewWriter(rdb, time.Minute, freeze.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()

	// Live, so the carry-forward is actually exercised.
	fired := now.Add(-5 * time.Minute)
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
	// place; once the upgrade tick has passed the ladders are scoped.
	if err := w.MarkHoldForWindow(ctx, asset, quote, longWindow, "0.124200000000",
		ladderDecision(), ladderState(fired, 2), time.Hour); err != nil {
		t.Fatalf("MarkHoldForWindow: %v", err)
	}
	now = now.Add(freeze.DefaultLadderGrace + time.Second)
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

	fired := time.Now().UTC()
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

// TestLoadStateForWindow_ColdWindowDoesNotAdoptEscalatedUpgradeLadder pins
// the adoption bound on an upgraded marker's unowned ladder. The snapshot
// stays live for its whole hold, and its Escalated bit is one ADR-0019
// never auto-releases, so a window that reaches the freeze step after the
// upgrade tick (it had been under the USD-volume floor) and adopted it
// would be held until a manual unfreeze on a bucket nothing was wrong with.
func TestLoadStateForWindow_ColdWindowDoesNotAdoptEscalatedUpgradeLadder(t *testing.T) {
	_, rdb := newRedis(t)
	now := time.Now().UTC()
	w, err := freeze.NewWriter(rdb, time.Minute, freeze.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()

	escalated := freeze.State{
		FiredAt:        now.Add(-2 * time.Hour),
		HoldUntil:      now.Add(30 * time.Minute),
		ExtensionsUsed: 4,
		Escalated:      true,
	}
	legacy := freeze.Marker{
		AssetID:  asset.String(),
		QuoteID:  quote.String(),
		Action:   anomaly.ActionFreeze,
		FrozenAt: now,
		State:    escalated,
	}
	body, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy marker: %v", err)
	}
	if err := rdb.Set(ctx, cachekeys.Freeze(asset, quote).String(), body, time.Hour).Err(); err != nil {
		t.Fatalf("seed legacy marker: %v", err)
	}

	// Upgrade tick: the 5m window re-marks, and a sibling that was running
	// the same pair-level ladder still reads it on this tick.
	if err := w.MarkHoldForWindow(ctx, asset, quote, shortWindow, "0.124200000000",
		ladderDecision(), escalated, time.Hour); err != nil {
		t.Fatalf("MarkHoldForWindow(5m) upgrade: %v", err)
	}
	sibling, ok, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(1h) on the upgrade tick: ok=%v err=%v", ok, err)
	}
	if !sibling.Escalated {
		t.Errorf("on the upgrade tick the 1h window read %+v, want the unowned escalated ladder", sibling)
	}

	// Later ticks: the 5m window keeps re-marking; the snapshot is still
	// inside its hold, so it is still carried forward.
	for i := 0; i < 3; i++ {
		now = now.Add(freeze.DefaultLadderGrace)
		escalated.HoldUntil = now.Add(30 * time.Minute)
		if err := w.MarkHoldForWindow(ctx, asset, quote, shortWindow, "0.124200000000",
			ladderDecision(), escalated, time.Hour); err != nil {
			t.Fatalf("MarkHoldForWindow(5m) tick %d: %v", i, err)
		}
	}

	cold, ok, err := w.LoadStateForWindow(ctx, asset, quote, 24*time.Hour)
	if err != nil {
		t.Fatalf("LoadStateForWindow(24h): %v", err)
	}
	if !ok {
		t.Error("presence is pair-wide: the 5m window's freeze keeps the marker, so the 24h window must read present")
	}
	if cold.Active() {
		t.Errorf("the 24h window, first reaching the freeze step %v after the upgrade, adopted %+v — "+
			"an escalated ladder it never ran, which only a manual unfreeze ends",
			3*freeze.DefaultLadderGrace, cold)
	}
	owner, _, err := w.LoadStateForWindow(ctx, asset, quote, shortWindow)
	if err != nil {
		t.Fatalf("LoadStateForWindow(5m): %v", err)
	}
	if !owner.Escalated {
		t.Errorf("the 5m window's own ladder read back %+v, want it still escalated", owner)
	}
}

func TestLoadStateForWindow_AbandonedOwnedLadderIsNotRehydrated(t *testing.T) {
	_, w, _ := seedShortLadderBeside(t, escalatedState(time.Now().UTC()))
	asset, quote := nativeUSD(t)
	st, present, err := w.LoadStateForWindow(context.Background(), asset, quote, shortWindow)
	if err != nil {
		t.Fatalf("LoadStateForWindow: %v", err)
	}
	if !present {
		t.Fatal("present = false; the pair's marker is still there")
	}
	if st.Active() || st.Escalated {
		t.Fatalf("5m window rehydrated an abandoned ladder: %+v; want the zero State", st)
	}
	long, _, err := w.LoadStateForWindow(context.Background(), asset, quote, longWindow)
	if err != nil {
		t.Fatalf("LoadStateForWindow(1h): %v", err)
	}
	if !long.Escalated {
		t.Fatalf("1h window lost its live escalated ladder: %+v", long)
	}
}

func TestMarkHoldForWindow_PrunesAnAbandonedSiblingLadder(t *testing.T) {
	now := time.Now().UTC()
	mr, w, key := seedShortLadderBeside(t, escalatedState(now))
	asset, quote := nativeUSD(t)
	if err := w.MarkHoldForWindow(context.Background(), asset, quote, longWindow, "0.1242",
		freezeDecision(), escalatedState(now), 30*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	raw, err := mr.Get(key)
	if err != nil {
		t.Fatalf("marker gone after re-mark: %v", err)
	}
	var got freeze.Marker
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	if st, kept := got.Ladders[shortWindow.String()]; kept {
		t.Fatalf("sibling re-mark carried the abandoned 5m ladder forward: %+v", st)
	}
	if !got.Ladders[longWindow.String()].Escalated {
		t.Fatalf("1h ladder missing from the re-marked marker: %+v", got.Ladders)
	}
}

// TestMarkHoldForWindow_NeverShortensASiblingsHold is the same invariant
// between two lifecycle writers. The marker has ONE TTL and carries every
// window's ladder; a 5m window re-marking with its own short remainder
// must not pull the expiry in under a 1h sibling whose window is not
// re-marking this tick.
func TestMarkHoldForWindow_NeverShortensASiblingsHold(t *testing.T) {
	mr, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, 0)
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
		freezeDecision(), freshState(now), 6*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}
	if ttl := mr.TTL(cachekeys.Freeze(asset, quote).String()); ttl < 25*time.Minute {
		t.Errorf("marker TTL = %s after the 5m window's write, want at least the 1h "+
			"window's remaining hold (25m)", ttl)
	}
}

// clearAfterReadCache lands an operator's freeze-unfreeze DEL between the
// retire's marker read and its write back.
type clearAfterReadCache struct {
	freeze.RedisCache
}

func (c clearAfterReadCache) Get(ctx context.Context, key string) *redis.StringCmd {
	cmd := c.RedisCache.Get(ctx, key)
	c.RedisCache.Del(ctx, key)
	return cmd
}

// TestRetireWindowLadder_DoesNotResurrectAClearedMarker: a marker cleared
// between the retire's read and its write must stay cleared. SET KEEPTTL
// on a key that no longer exists creates it with NO expiry, so the write
// back would bring the freeze back permanently, past the operator's clear.
func TestRetireWindowLadder_DoesNotResurrectAClearedMarker(t *testing.T) {
	mr, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, time.Minute)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	fired := time.Now().UTC()
	for _, window := range []time.Duration{shortWindow, longWindow} {
		if err := w.MarkHoldForWindow(ctx, asset, quote, window, "0.124200000000",
			ladderDecision(), ladderState(fired, 0), time.Hour); err != nil {
			t.Fatalf("MarkHoldForWindow(%v): %v", window, err)
		}
	}

	racing, err := freeze.NewWriter(clearAfterReadCache{RedisCache: rdb}, time.Minute)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := racing.RetireWindowLadder(ctx, asset, quote, shortWindow); err != nil {
		t.Fatalf("RetireWindowLadder after a concurrent clear = %v, want nil (already cleared)", err)
	}
	key := cachekeys.Freeze(asset, quote).String()
	if mr.Exists(key) {
		t.Fatalf("the cleared marker was written back (TTL %v) — the operator's "+
			"unfreeze was undone", mr.TTL(key))
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
