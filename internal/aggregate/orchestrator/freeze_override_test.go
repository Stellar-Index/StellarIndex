package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestFreezeLifecycle_RefireAfterOverrideReturnsEscalated — the operator
// force-unfreezes an escalated pair and the anomaly is still live. The
// override used to zero the state, so the re-fire was a fresh 10-minute hold
// with extensions_used=0 that would not page again for two hours.
func TestFreezeLifecycle_RefireAfterOverrideReturnsEscalated(t *testing.T) {
	f := newFreezeFixture(t)
	escalated := freeze.State{
		FiredAt:        f.now.Add(-3 * time.Hour),
		HoldUntil:      f.now.Add(20 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions,
		Escalated:      true,
	}
	f.orch.freezeStates[f.stateKey()] = escalated
	f.marker.present, f.marker.state = true, escalated
	beforeEsc := testutil.ToFloat64(obs.AnomalyFreezeEscalatedTotal)
	beforeRefire := testutil.ToFloat64(obs.AnomalyFreezeRefiredAfterOverrideTotal)

	// Override: the marker goes; the next bucket publishes, as asked.
	f.marker.present = false
	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	if f.state().Active() {
		t.Fatalf("setup: the override did not release: %+v", f.state())
	}

	// Still anomalous one bucket later.
	f.feed(t, manipQuoteAmount*3/2, "soroswap")
	f.tick(t, closedBucket)
	st := f.state()
	if !st.Active() || !st.Escalated || st.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Fatalf("re-fire after override = %+v, want the escalated ladder resumed", st)
	}
	if got := testutil.ToFloat64(obs.AnomalyFreezeEscalatedTotal) - beforeEsc; got != 1 {
		t.Errorf("escalation counter delta = %v, want 1: the P1 must page again", got)
	}
	if got := testutil.ToFloat64(obs.AnomalyFreezeRefiredAfterOverrideTotal) - beforeRefire; got != 1 {
		t.Errorf("refired-after-override counter delta = %v, want 1", got)
	}
}

// TestFreezeLifecycle_ReleaseModeTellsOperatorFromLapse — a live freeze
// whose marker and ladder are gone was always counted as mode="operator",
// so a lapse nobody performed polluted the series the on-call reads as the
// manual-unfreeze rate. freeze-unfreeze's tombstone is what separates them.
func TestFreezeLifecycle_ReleaseModeTellsOperatorFromLapse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tombstone bool
		want      string
	}{
		{name: "freeze_unfreeze", tombstone: true, want: "operator"},
		{name: "lapse", tombstone: false, want: "lapsed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWiredFreezeFixture(t)
			w, ok := f.orch.cfg.FreezeWriter.(*freeze.Writer)
			if !ok {
				t.Fatal("setup: the wired fixture must use freeze.Writer")
			}
			f.feed(t, manipQuoteAmount, "soroswap")
			f.advance(t, closedBucket)
			if !f.state().Active() {
				t.Fatal("setup: freeze did not fire")
			}

			ctx := context.Background()
			if tc.tombstone {
				if err := w.RecordOverride(ctx, f.pair.Base, f.pair.Quote, "oncall", "verified by hand"); err != nil {
					t.Fatalf("RecordOverride: %v", err)
				}
			}
			if err := w.Clear(ctx, f.pair.Base, f.pair.Quote); err != nil {
				t.Fatalf("Clear: %v", err)
			}
			before := map[string]float64{}
			for _, m := range []string{"operator", "lapsed"} {
				before[m] = testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues(m))
			}

			f.advance(t, closedBucket)
			if f.state().Active() {
				t.Fatalf("setup: the missing marker did not release: %+v", f.state())
			}
			for _, m := range []string{"operator", "lapsed"} {
				want := 0.0
				if m == tc.want {
					want = 1
				}
				if got := testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues(m)) - before[m]; got != want {
					t.Errorf("AnomalyFreezeReleasedTotal{%s} delta = %v, want %v", m, got, want)
				}
			}
		})
	}
}
