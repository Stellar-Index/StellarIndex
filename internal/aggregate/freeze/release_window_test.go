package freeze_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// ─── ReleaseWindow asks the RECORD whether a sibling is frozen ────

// TestReleaseWindow_KeepsTheMarkerForASiblingsLadder: the caller believes
// it is the pair's last frozen window — but only because the 1h window has
// not been evaluated in its process. The marker says otherwise, and the
// marker wins: the 5m ladder goes, everything the 1h window owns stays, in
// Redis and in the durable record.
func TestReleaseWindow_KeepsTheMarkerForASiblingsLadder(t *testing.T) {
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

	kept, err := w.ReleaseWindow(ctx, asset, quote, shortWindow)
	if err != nil {
		t.Fatalf("ReleaseWindow: %v", err)
	}
	if !kept {
		t.Error("ReleaseWindow reported the marker cleared while the 1h window's ladder was in it")
	}
	key := cachekeys.Freeze(asset, quote).String()
	if !mr.Exists(key) {
		t.Fatal("the marker is gone: a sibling's escalated freeze ended because the 5m window released")
	}
	if got, _, _ := w.LoadStateForWindow(ctx, asset, quote, shortWindow); got.Active() {
		t.Errorf("the released 5m window still owns a ladder: %+v", got)
	}

	// And the durable record agrees, which is what a Redis loss falls to.
	mr.FlushAll()
	gotLong, ok, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
	if err != nil || !ok || !gotLong.Escalated {
		t.Errorf("1h durable ladder after the sibling's release = (%+v, ok=%v, err=%v), want it "+
			"escalated and intact — the release retired the whole durable record", gotLong, ok, err)
	}
	if got, _, _ := w.LoadStateForWindow(ctx, asset, quote, shortWindow); got.Active() {
		t.Errorf("the released 5m window's durable ladder survived: %+v", got)
	}
}

// TestReleaseWindow_ClearsWhenNoSiblingIsRecorded: with no other window in
// the record, releasing IS clearing — flags.frozen drops at once and the
// durable ladder is retired, so a restart cannot resurrect the freeze.
func TestReleaseWindow_ClearsWhenNoSiblingIsRecorded(t *testing.T) {
	mr, rdb := newRedis(t)
	store := newWindowedFakeLadderStore()
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(store, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	if err := w.MarkHoldForWindow(ctx, asset, quote, shortWindow, "0.1242",
		freezeDecision(), freshState(time.Now().UTC()), 14*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}

	kept, err := w.ReleaseWindow(ctx, asset, quote, shortWindow)
	if err != nil || kept {
		t.Fatalf("ReleaseWindow = (kept=%v, err=%v), want (false, nil)", kept, err)
	}
	if mr.Exists(cachekeys.Freeze(asset, quote).String()) {
		t.Error("the pair's only frozen window released and the marker is still there")
	}
	if _, ok, _ := w.LoadStateForWindow(ctx, asset, quote, shortWindow); ok {
		t.Error("the durable ladder outlived the release: a restart would re-freeze a recovered pair")
	}

	// Releasing an already-absent marker (the operator override deleted it
	// first) stays an idempotent clear.
	if kept, err := w.ReleaseWindow(ctx, asset, quote, shortWindow); err != nil || kept {
		t.Errorf("ReleaseWindow on an absent marker = (kept=%v, err=%v), want (false, nil)", kept, err)
	}
}

// getFailingCache fails every Get and records whether anything was deleted.
type getFailingCache struct {
	freeze.RedisCache
	deleted bool
}

func (c *getFailingCache) Get(ctx context.Context, _ string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx)
	cmd.SetErr(errors.New("redis: connection refused"))
	return cmd
}

func (c *getFailingCache) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	c.deleted = true
	return c.RedisCache.Del(ctx, keys...)
}

// TestReleaseWindow_DoesNotClearAMarkerItCannotRead: not knowing whether a
// sibling window is frozen is no ground for unfreezing it. The marker is
// left to its TTL and the caller is told.
func TestReleaseWindow_DoesNotClearAMarkerItCannotRead(t *testing.T) {
	_, rdb := newRedis(t)
	cache := &getFailingCache{RedisCache: rdb}
	w, err := freeze.NewWriter(cache, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)

	kept, err := w.ReleaseWindow(context.Background(), asset, quote, shortWindow)
	if err == nil {
		t.Fatal("ReleaseWindow swallowed the marker read failure")
	}
	if !kept || cache.deleted {
		t.Errorf("ReleaseWindow = kept=%v deleted=%v on an unreadable marker, want it left alone",
			kept, cache.deleted)
	}
}
