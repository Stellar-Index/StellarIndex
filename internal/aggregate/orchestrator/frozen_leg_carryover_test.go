package orchestrator

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestFrozenLeg_DoesNotOutliveTheHold bounds the carry-over. The
// in-memory ladder of a window that stops being evaluated is never
// advanced and never released, so reading it without a bound would refuse
// every chain through that pair for as long as its window stayed empty —
// long after the marker and the last-known-good value it protects have
// both expired out of Redis. The guard covers exactly the span the LKG
// can still be read back: the hold plus the marker grace.
func TestFrozenLeg_DoesNotOutliveTheHold(t *testing.T) {
	leg := xlmUsdtPair(t)
	window := 5 * time.Minute
	cache, _ := newTestRedis(t)
	o := New(nil, cache, Config{
		Pairs:   []canonical.Pair{leg},
		Windows: []time.Duration{window},
	})
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	o.clock = func() time.Time { return now }
	grace := o.cfg.Phase2Thresholds.Lifecycle.WithDefaults().MarkerGrace

	st := o.freezeStates[leg.String()+":"+window.String()]
	st.FiredAt = now.Add(-20 * time.Minute)
	st.HoldUntil = now.Add(-time.Minute) // hold over, inside the grace
	o.freezeStates[leg.String()+":"+window.String()] = st
	if !o.frozenLeg(leg, window) {
		t.Error("a leg inside its marker grace must still be refused: its LKG is still in cache")
	}

	now = st.HoldUntil.Add(grace + time.Second)
	if o.frozenLeg(leg, window) {
		t.Error("a leg whose hold and grace have both lapsed is still refused — its marker and " +
			"LKG are gone, and nothing will ever clear this in-memory ladder while the " +
			"window stays empty")
	}

	// A different window of the same pair is not implicated.
	now = st.HoldUntil.Add(-time.Minute)
	if o.frozenLeg(leg, time.Hour) {
		t.Error("the 1h window read as frozen on the 5m window's ladder")
	}
}
