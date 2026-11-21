package freeze_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// lapsedBy returns st with its hold ended `ago` in the past, as an owner outage longer than hold plus grace leaves it.
func lapsedBy(st freeze.State, ago time.Duration) freeze.State {
	st.HoldUntil = time.Now().UTC().Add(-ago)
	return st
}

// seedOutage leaves a 1h ESCALATED ladder and a 24h ordinary ladder both lapsed by an outage, and a live 5m ladder,
// in the durable record only: the marker expired while the aggregator was down.
func seedOutage(t *testing.T, store *windowedFakeLadderStore, asset, quote canonical.Asset) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	ordinary := freshState(now)
	ordinary.FiredAt = now.Add(-2 * time.Hour)
	for window, st := range map[time.Duration]freeze.State{
		longWindow:     lapsedBy(escalatedState(now), 36*time.Minute),
		24 * time.Hour: lapsedBy(ordinary, 36*time.Minute),
		shortWindow:    freshState(now),
	} {
		if err := store.SaveWindowLadder(ctx, asset, quote, window, st); err != nil {
			t.Fatalf("seed %s: %v", window, err)
		}
	}
}

// TestWriter_SiblingRebuildAfterOutageKeepsEscalatedWindow: whichever write rebuilds the marker after an outage, the
// escalated 1h window must read back escalated, because only an operator ends an escalated freeze; a lapsed ordinary
// ladder is still pruned, and freeze-unfreeze still ends everything.
func TestWriter_SiblingRebuildAfterOutageKeepsEscalatedWindow(t *testing.T) {
	asset, quote := nativeUSD(t)
	first := map[string]func(ctx context.Context, w *freeze.Writer) error{
		"window_remark": func(ctx context.Context, w *freeze.Writer) error {
			return w.MarkHoldForWindow(ctx, asset, quote, shortWindow, "0.1242",
				freezeDecision(), freshState(time.Now().UTC()), 14*time.Minute)
		},
		"lifecycle_free_mark": func(ctx context.Context, w *freeze.Writer) error {
			return w.Mark(ctx, asset, quote, "0.1242", freezeDecision())
		},
	}
	for name, write := range first {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			_, rdb := newRedis(t)
			store := newWindowedFakeLadderStore()
			seedOutage(t, store, asset, quote)
			w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(store, freeze.DefaultLadderGrace))
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			for tick := 0; tick < 2; tick++ {
				if err := write(ctx, w); err != nil {
					t.Fatalf("sibling write %d: %v", tick, err)
				}
				st, present, err := w.LoadStateForWindow(ctx, asset, quote, longWindow)
				if err != nil || !present || !st.Escalated || st.ExtensionsUsed != freeze.DefaultMaxExtensions {
					t.Fatalf("1h after sibling write %d = (%+v, present=%v, err=%v), want its escalated ladder: "+
						"the rebuilt marker dropped it and the 1h window publishes the withheld price", tick, st, present, err)
				}
			}
			if st, _, err := w.LoadStateForWindow(ctx, asset, quote, 24*time.Hour); err != nil || st.Active() {
				t.Errorf("24h = (%+v, err=%v), want a lapsed ordinary ladder pruned, not resurrected", st, err)
			}

			if err := w.Clear(ctx, asset, quote); err != nil {
				t.Fatalf("operator Clear: %v", err)
			}
			store.close(asset, quote)
			if _, present, err := w.LoadStateForWindow(ctx, asset, quote, longWindow); err != nil || present {
				t.Errorf("1h after freeze-unfreeze = (present=%v, err=%v), want (false, nil)", present, err)
			}
		})
	}
}

// TestReleaseWindow_SiblingReleaseKeepsALapsedEscalatedWindow: a sibling's auto-release must not clear the pair while
// an escalated window's ladder is lapsed (an outage, or a window that stopped ticking): the durable record and the open
// freeze_events row survive, and the escalated window still reads escalated.
func TestReleaseWindow_SiblingReleaseKeepsALapsedEscalatedWindow(t *testing.T) {
	asset, quote := nativeUSD(t)
	key := cachekeys.Freeze(asset, quote).String()
	cases := map[string]func(t *testing.T, w *freeze.Writer, rdbSet func(string, string)){
		// The marker was rebuilt by the 5m window after an outage.
		"after_outage": func(t *testing.T, w *freeze.Writer, _ func(string, string)) {
			if err := w.MarkHoldForWindow(context.Background(), asset, quote, shortWindow, "0.1242",
				freezeDecision(), freshState(time.Now().UTC()), 14*time.Minute); err != nil {
				t.Fatalf("MarkHoldForWindow(5m): %v", err)
			}
		},
		// The marker survived and carries the 1h ladder, lapsed because the window stopped advancing it.
		"marker_carries_lapsed_ladder": func(t *testing.T, _ *freeze.Writer, set func(string, string)) {
			now := time.Now().UTC()
			body, err := json.Marshal(freeze.Marker{
				AssetID: asset.String(), QuoteID: quote.String(), Windowed: true,
				Ladders: map[string]freeze.State{
					longWindow.String():  lapsedBy(escalatedState(now), 36*time.Minute),
					shortWindow.String(): freshState(now),
				},
			})
			if err != nil {
				t.Fatalf("marshal marker: %v", err)
			}
			set(key, string(body))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			mr, rdb := newRedis(t)
			store := newWindowedFakeLadderStore()
			seedOutage(t, store, asset, quote)
			sink := &recoveringSink{}
			w, err := freeze.NewWriter(rdb, 0,
				freeze.WithLadderStore(store, freeze.DefaultLadderGrace), freeze.WithEventSink(sink))
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			setup(t, w, func(k, v string) {
				if err := mr.Set(k, v); err != nil {
					t.Fatalf("seed marker: %v", err)
				}
			})

			kept, err := w.ReleaseWindow(ctx, asset, quote, shortWindow)
			if err != nil || !kept {
				t.Fatalf("ReleaseWindow(5m) = (kept=%v, err=%v), want (true, nil): the escalated 1h window is still frozen", kept, err)
			}
			if got := store.windows[store.key(asset, quote)][longWindow]; !got.Escalated {
				t.Errorf("1h durable ladder after the 5m release = %+v, want it escalated: Clear NULLed the record", got)
			}
			if n := len(sink.fakeRecoverer.calls); n != 0 {
				t.Errorf("the 5m release closed the open freeze_events row (%d calls): an escalation ended with no operator", n)
			}
			if st, present, err := w.LoadStateForWindow(ctx, asset, quote, longWindow); err != nil || !present || !st.Escalated {
				t.Errorf("1h after the 5m release = (%+v, present=%v, err=%v), want escalated", st, present, err)
			}
		})
	}
}
