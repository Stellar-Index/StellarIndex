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

// ─── Mark sets the serving flag and NOTHING a lifecycle owns ─────
//
// `freeze:<asset>:<quote>` has two writers. The ADR-0019 lifecycle
// (MarkHoldForWindow) owns the ladders inside it and the TTL that keeps
// them alive for the hold. The triangulated-composite refusal
// (inheritLegFreeze → Mark) owns neither: it only needs the pair to carry
// flags.frozen for a few minutes. Its targets are themselves members of
// the aggregator's pair set, and its call sits on the ErrNoRoute branch
// AHEAD of the guard that protects a self-frozen target — so Mark does
// land on markers a live ladder owns. These tests pin what it may not do
// to them.

func inheritedDecision() anomaly.Decision {
	return anomaly.Decision{
		Action: anomaly.ActionFreeze,
		Class:  anomaly.ClassCrypto,
		Reason: "triangulation:leg_frozen leg=native/fiat:USD window=1h0m0s",
	}
}

// TestMark_NeverShortensALiveLifecycleTTL: the lifecycle wrote the marker
// with `remaining hold + grace` (tens of minutes). Mark's flat TTL used to
// replace it, so on any tick the owning window did not re-mark — it
// returns early on an empty or thin bucket — the marker of an ESCALATED
// freeze was five minutes from lapsing.
func TestMark_NeverShortensALiveLifecycleTTL(t *testing.T) {
	mr, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, time.Minute) // flat TTL: 1 minute
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	escalated := escalatedState(time.Now().UTC()) // hold has 25 minutes left

	if err := w.MarkHoldForWindow(ctx, asset, quote, longWindow, "0.1242",
		freezeDecision(), escalated, 30*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow: %v", err)
	}
	if err := w.Mark(ctx, asset, quote, "0.1242", inheritedDecision()); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	key := cachekeys.Freeze(asset, quote).String()
	if ttl := mr.TTL(key); ttl < 25*time.Minute {
		t.Errorf("marker TTL after Mark = %s, want at least the ladder's remaining hold "+
			"(25m) — the stateless writer truncated a live ADR-0019 hold to its flat TTL", ttl)
	}
	got, ok, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
	if err != nil || !ok || !got.Escalated {
		t.Errorf("1h ladder after Mark = (%+v, ok=%v, err=%v), want it untouched", got, ok, err)
	}

	// The marker still describes the freeze its OWNER recorded, not the
	// inherited refusal: an operator dumping freeze:* mid-incident reads
	// why this pair escalated.
	raw, gerr := mr.Get(key)
	if gerr != nil {
		t.Fatalf("read marker: %v", gerr)
	}
	var marker freeze.Marker
	if err := json.Unmarshal([]byte(raw), &marker); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	if marker.Reason != freezeDecision().Reason || !marker.State.Escalated {
		t.Errorf("marker after Mark: reason=%q state=%+v, want the lifecycle owner's "+
			"(%q, escalated) — Mark rewrote a marker it does not own",
			marker.Reason, marker.State, freezeDecision().Reason)
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

// TestMark_PreservesALegacyMarkersLadder: a marker written before
// per-window ladders existed keeps its one ladder in the pair-level State
// field, which is what every window rehydrates from. Mark used to rewrite
// that field to zero.
func TestMark_PreservesALegacyMarkersLadder(t *testing.T) {
	_, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, time.Minute)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	escalated := escalatedState(time.Now().UTC())

	// The unscoped MarkHold writes exactly the pre-window marker shape.
	if err := w.MarkHold(ctx, asset, quote, "0.1242", freezeDecision(), escalated, 30*time.Minute); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}
	if err := w.Mark(ctx, asset, quote, "0.1242", inheritedDecision()); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	got, ok, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow = (ok=%v, err=%v), want present", ok, err)
	}
	if !got.Escalated || got.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Errorf("ladder after Mark = %+v, want the escalated ladder the marker carried — "+
			"a cold window now reads 'present, never frozen' and publishes", got)
	}
}

// TestMark_KeepsALegacyMarkersOwnerRecord: on a legacy marker the
// pair-level State IS the live ladder, so Mark must leave the whole
// marker as its lifecycle owner wrote it — reason and ladder — not
// relabel an escalated freeze as an inherited refusal.
func TestMark_KeepsALegacyMarkersOwnerRecord(t *testing.T) {
	mr, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, time.Minute)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	if err := w.MarkHold(ctx, asset, quote, "0.1242", freezeDecision(), escalatedState(time.Now().UTC()), 30*time.Minute); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}
	if err := w.Mark(ctx, asset, quote, "0.1242", inheritedDecision()); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	key := cachekeys.Freeze(asset, quote).String()
	raw, gerr := mr.Get(key)
	if gerr != nil {
		t.Fatalf("read marker: %v", gerr)
	}
	var marker freeze.Marker
	if err := json.Unmarshal([]byte(raw), &marker); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	if marker.Windowed || marker.Reason != freezeDecision().Reason || !marker.State.Escalated {
		t.Errorf("legacy marker after Mark: windowed=%v reason=%q state=%+v, want the owner's "+
			"unwindowed marker (%q, escalated)", marker.Windowed, marker.Reason, marker.State, freezeDecision().Reason)
	}
	if ttl := mr.TTL(key); ttl < 25*time.Minute {
		t.Errorf("legacy marker TTL after Mark = %s, want at least the remaining hold (25m)", ttl)
	}
}

// TestMark_AfterRedisLossDoesNotHideTheDurableLadder: Redis lost the
// marker, and the inherited refusal re-creates it BEFORE the target's own
// window next reaches the freeze step. From then on the marker is present,
// so the durable ladder is never consulted again — the marker Mark writes
// has to carry it, or the target's cold window reads a present marker with
// no ladder and publishes the bucket an escalated freeze was withholding.
func TestMark_AfterRedisLossDoesNotHideTheDurableLadder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func() freeze.LadderStore
	}{
		{
			// A pre-0163 durable row: one pair-level ladder, no owner.
			name:  "pair-level durable ladder",
			store: func() freeze.LadderStore { return newFakeLadderStore() },
		},
		{
			name:  "per-window durable ladders",
			store: func() freeze.LadderStore { return newWindowedFakeLadderStore() },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr, rdb := newRedis(t)
			w, err := freeze.NewWriter(rdb, time.Minute, freeze.WithLadderStore(tc.store(), 0))
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			asset, quote := nativeUSD(t)
			ctx := context.Background()
			if err := w.MarkHoldForWindow(ctx, asset, quote, longWindow, "0.1242",
				freezeDecision(), escalatedState(time.Now().UTC()), 30*time.Minute); err != nil {
				t.Fatalf("MarkHoldForWindow: %v", err)
			}
			mr.FlushAll()

			if err := w.Mark(ctx, asset, quote, "0.1242", inheritedDecision()); err != nil {
				t.Fatalf("Mark: %v", err)
			}
			got, ok, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
			if err != nil || !ok {
				t.Fatalf("LoadStateForWindow = (ok=%v, err=%v), want present", ok, err)
			}
			if !got.Escalated {
				t.Errorf("1h ladder = %+v, want the escalated durable ladder — Mark re-created "+
					"the marker without it, so the durable record is now unreachable", got)
			}
		})
	}
}
