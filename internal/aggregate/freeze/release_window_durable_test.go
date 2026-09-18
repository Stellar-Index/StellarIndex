package freeze_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// ─── ReleaseWindow falls back to the DURABLE record, not to Clear ───
//
// The marker is only the first place the record lives. When Redis has lost
// it — the one situation migration 0163's per-window durable ladders exist
// for — "the marker names no sibling" is not "no sibling is frozen", and a
// release that read it that way retired the whole durable record: a
// recovering 5m window ended an ESCALATED 1h sibling's freeze, which
// ADR-0019 holds "until manual unfreeze".

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

// TestReleaseWindow_AsksTheDurableRecordWhenTheMarkerCannotAnswer: for
// every shape of marker that names no sibling — gone, undecodable, or
// written before per-window ladders — the 5m window's release must leave
// the escalated 1h window's durable ladder exactly as it was, retire only
// its own, and report the freeze kept.
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

// TestReleaseWindow_KeepsALiveUnownedDurableLadder: a row written before
// 0163 carries one pair-level ladder with no recorded owner. It may be a
// sibling's, so a window's release must not retire it either.
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

// TestReleaseWindow_DoesNotClearADurableRecordItCannotRead: with the marker
// gone the durable record is the only authority, and a release that cannot
// read it does not get to retire it. The rehydrate reads degrade a store
// error to "absent" because there that invents nothing; here "absent" is
// the answer that destroys every sibling's ladder.
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

// TestReleaseWindow_StillClearsOnceTheRecordIsRetired pins the other side:
// the operator override (`stellarindex-ops freeze-unfreeze`) retires the
// durable record itself, so the release each window then makes finds no
// sibling anywhere and is the same idempotent clear as before. Consulting
// the durable record must not make an override fail to stick.
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
