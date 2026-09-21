package forex

import (
	"context"
	"testing"
)

// TestRefreshOnce_ColdStartWithoutNamesStillInstallsSnapshot is the
// cold-start arm of a primary outage: the process has never installed a
// snapshot, the rates fetch succeeds (here directly; in production via
// the ECB fallback) and the primary's names endpoint errors. Names are
// static display labels, so the refresh must still install the rates —
// labelled by ticker — rather than leave the feed empty until the
// primary returns. The warm path already reuses cached names; this
// pins the one-time cold path that used to return before cache.Set.
func TestRefreshOnce_ColdStartWithoutNamesStillInstallsSnapshot(t *testing.T) {
	up := &fakeMassive{
		current: map[string]float64{"EUR": 0.92, "UZS": 11800},
		history: map[string]float64{"EUR": 0.92, "UZS": 11790},
		names:   nil, // writeTickers emits an empty list -> CurrencyNames errors
	}
	writer := &recordingFXWriter{}
	w := newGuardedWorker(t, up, writer)
	w.refreshOnce(context.Background())

	if w.cache.Latest() == nil {
		t.Fatal("no snapshot installed: a cold start with the names endpoint down left the feed empty")
	}
	for ticker, want := range map[string]float64{"EUR": 0.92, "UZS": 11800} {
		got, ok := servedRate(t, w.cache, ticker)
		if !ok || got != want {
			t.Errorf("served %s = %v (present=%v), want %v — rates in hand must be served even without names",
				ticker, got, ok, want)
		}
	}
	if len(writer.batches) == 0 {
		t.Error("nothing persisted to fx_quotes on the cold-start path")
	}
}
