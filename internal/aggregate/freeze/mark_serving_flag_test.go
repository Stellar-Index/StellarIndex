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

// Mark sets the serving flag and NOTHING a lifecycle owns: the composite refusal (inheritLegFreeze) calls Mark on pairs
// whose markers a live ladder may own, so these tests pin what Mark may not do to them.

func inheritedDecision() anomaly.Decision {
	return anomaly.Decision{
		Action: anomaly.ActionFreeze,
		Class:  anomaly.ClassCrypto,
		Reason: "triangulation:leg_frozen leg=native/fiat:USD window=1h0m0s",
	}
}

// TestMark_NeverShortensALiveLifecycleTTL: Mark's flat TTL must not replace the lifecycle's remaining hold plus grace, or an ESCALATED freeze lapses in minutes.
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

// TestMark_PreservesALegacyMarkersLadder: Mark must not zero the pair-level State a pre-Ladders marker keeps its one ladder in.
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

// TestMark_KeepsALegacyMarkersOwnerRecord: on a legacy marker Mark leaves reason and ladder as the owner wrote them, not relabelling an escalated freeze as inherited.
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

// TestMark_AfterRedisLossDoesNotHideTheDurableLadder: a Mark that re-creates a lost marker must carry the durable ladder, since a present marker is never rechecked against it.
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
