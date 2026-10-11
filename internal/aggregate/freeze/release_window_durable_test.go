package freeze_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// ReleaseWindow falls back to the DURABLE record, not to Clear: when Redis lost the marker, "no sibling named" is not
// "no sibling frozen", and treating it so let a recovering 5m window end an ESCALATED 1h freeze.

// bothWindowsFrozen freezes the 1h window (escalated) and the 5m window
// (fresh) through the production writer shape: a ladder store is wired.
func bothWindowsFrozen(t *testing.T) (*miniredis.Miniredis, *freeze.Writer, *windowedFakeLadderStore) {
	t.Helper()
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
	return mr, w, store
}

// TestReleaseWindow_AsksTheDurableRecordWhenTheMarkerCannotAnswer: for a gone, undecodable or pre-Ladders marker, the 5m release retires only its own durable ladder and reports the freeze kept.
func TestReleaseWindow_AsksTheDurableRecordWhenTheMarkerCannotAnswer(t *testing.T) {
	asset, quote := nativeUSD(t)
	key := cachekeys.Freeze(asset, quote).String()
	legacy, err := json.Marshal(freeze.Marker{AssetID: asset.String(), QuoteID: quote.String()})
	if err != nil {
		t.Fatalf("marshal legacy marker: %v", err)
	}
	rebuilt, err := json.Marshal(freeze.Marker{
		AssetID: asset.String(), QuoteID: quote.String(), Windowed: true,
		Ladders: map[string]freeze.State{shortWindow.String(): freshState(time.Now().UTC())},
	})
	if err != nil {
		t.Fatalf("marshal rebuilt marker: %v", err)
	}
	cases := []struct {
		name    string
		degrade func(t *testing.T, mr *miniredis.Miniredis)
	}{
		{"marker absent", func(_ *testing.T, mr *miniredis.Miniredis) { mr.FlushAll() }},
		{"marker undecodable", func(t *testing.T, mr *miniredis.Miniredis) {
			if err := mr.Set(key, "{not json"); err != nil {
				t.Fatalf("corrupt marker: %v", err)
			}
		}},
		{"marker pre-window", func(t *testing.T, mr *miniredis.Miniredis) {
			if err := mr.Set(key, string(legacy)); err != nil {
				t.Fatalf("legacy marker: %v", err)
			}
		}},
		// Redis lost the marker and it was rebuilt on a tick the durable
		// read failed: windowed, but it only knows the window that wrote it.
		{"marker rebuilt without the sibling", func(t *testing.T, mr *miniredis.Miniredis) {
			if err := mr.Set(key, string(rebuilt)); err != nil {
				t.Fatalf("rebuilt marker: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr, w, _ := bothWindowsFrozen(t)
			ctx := context.Background()
			tc.degrade(t, mr)

			kept, err := w.ReleaseWindow(ctx, asset, quote, shortWindow)
			if err != nil {
				t.Fatalf("ReleaseWindow: %v", err)
			}
			if !kept {
				t.Error("ReleaseWindow reported the freeze cleared while the durable record " +
					"still held the 1h window's escalated ladder")
			}

			// The durable record is what every cold window reads next.
			mr.FlushAll()
			got, ok, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
			if err != nil || !ok || !got.Escalated || got.ExtensionsUsed != freeze.DefaultMaxExtensions {
				t.Errorf("1h durable ladder after the 5m release = (%+v, ok=%v, err=%v), want it "+
					"escalated at rung %d: an escalated freeze ended because a sibling recovered",
					got, ok, err, freeze.DefaultMaxExtensions)
			}
			if got, _, _ := w.LoadStateForWindow(ctx, asset, quote, shortWindow); got.Active() {
				t.Errorf("the released 5m window's durable ladder survived: %+v", got)
			}
		})
	}
}

// TestReleaseWindow_KeepsALiveUnownedDurableLadder: a pre-0163 pair-level ladder may be a sibling's, so a window's release must not retire it.
func TestReleaseWindow_KeepsALiveUnownedDurableLadder(t *testing.T) {
	_, rdb := newRedis(t)
	store := newWindowedFakeLadderStore()
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(store, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	unowned := escalatedState(time.Now().UTC())
	if err := store.SaveLadder(ctx, asset, quote, unowned); err != nil {
		t.Fatalf("seed pre-0163 ladder: %v", err)
	}

	kept, err := w.ReleaseWindow(ctx, asset, quote, shortWindow)
	if err != nil || !kept {
		t.Fatalf("ReleaseWindow = (kept=%v, err=%v), want (true, nil)", kept, err)
	}
	got, ok, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
	if err != nil || !ok || !got.Escalated {
		t.Errorf("unowned durable ladder after a window's release = (%+v, ok=%v, err=%v), "+
			"want it escalated and intact", got, ok, err)
	}
}

// TestReleaseWindow_DoesNotClearADurableRecordItCannotRead: with the marker gone the durable record is the only authority, so an unreadable one must not be retired.
func TestReleaseWindow_DoesNotClearADurableRecordItCannotRead(t *testing.T) {
	mr, w, store := bothWindowsFrozen(t)
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	mr.FlushAll()
	store.err = errors.New("pq: connection refused")

	kept, err := w.ReleaseWindow(ctx, asset, quote, shortWindow)
	if err == nil || !kept {
		t.Fatalf("ReleaseWindow = (kept=%v, err=%v) on an unreadable durable record, "+
			"want it kept and the failure reported", kept, err)
	}

	store.err = nil
	got, ok, lerr := w.LoadStateForWindow(ctx, asset, quote, longWindow)
	if lerr != nil || !ok || !got.Escalated {
		t.Errorf("1h durable ladder once the store is back = (%+v, ok=%v, err=%v), "+
			"want it escalated and intact", got, ok, lerr)
	}
}

// TestReleaseWindow_StillClearsOnceTheRecordIsRetired: after the operator override retires the durable record, each window's release is the same idempotent clear.
func TestReleaseWindow_StillClearsOnceTheRecordIsRetired(t *testing.T) {
	mr, w, _ := bothWindowsFrozen(t)
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	if err := w.Clear(ctx, asset, quote); err != nil { // what freeze-unfreeze calls
		t.Fatalf("Clear: %v", err)
	}

	for _, window := range []time.Duration{shortWindow, longWindow} {
		kept, err := w.ReleaseWindow(ctx, asset, quote, window)
		if err != nil || kept {
			t.Errorf("ReleaseWindow(%s) after the override = (kept=%v, err=%v), want (false, nil)",
				window, kept, err)
		}
		if _, ok, _ := w.LoadStateForWindow(ctx, asset, quote, window); ok {
			t.Errorf("window %s still has a ladder after the operator override", window)
		}
	}
	if mr.Exists(cachekeys.Freeze(asset, quote).String()) {
		t.Error("the marker came back after the operator override")
	}
}

func TestReleaseWindow_AbandonedSiblingDoesNotKeepThePairFrozen(t *testing.T) {
	mr, w, key := seedShortLadderBeside(t, freshState(time.Now().UTC()))
	asset, quote := nativeUSD(t)
	kept, err := w.ReleaseWindow(context.Background(), asset, quote, longWindow)
	if err != nil {
		t.Fatalf("ReleaseWindow: %v", err)
	}
	if kept {
		t.Fatal("ReleaseWindow kept the pair frozen on an abandoned sibling ladder")
	}
	if mr.Exists(key) {
		t.Fatal("marker still present after the last live window released")
	}
}

// ─── ReleaseWindow asks the RECORD whether a sibling is frozen ────

// TestReleaseWindow_KeepsTheMarkerForASiblingsLadder: the marker wins over the caller's last-window belief; the 5m ladder goes, the 1h window's state stays in Redis and the durable record.
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

// TestReleaseWindow_ClearsWhenNoSiblingIsRecorded: with no other window recorded, releasing IS clearing, including the durable ladder.
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

// TestReleaseWindow_DoesNotClearAMarkerItCannotRead: not knowing whether a sibling is frozen is no ground to unfreeze it; the marker is left to its TTL.
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
