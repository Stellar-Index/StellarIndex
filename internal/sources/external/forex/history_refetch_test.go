package forex

import (
	"context"
	"testing"
	"time"
)

// TestRefreshOnce_EmptyHistoryRefetchKeepsTheHeldSeries: the daily refetch replaces the series the heal and the confirm veto read;
// a refetch that came back with nothing must not leave them with no evidence.
func TestRefreshOnce_EmptyHistoryRefetchKeepsTheHeldSeries(t *testing.T) {
	// The primary is down for rates and history alike; the standby still supplies current rates.
	up := &fakeMassive{names: guardedNames, groupedStatus: 500}
	w := newGuardedWorker(t, up, nil).WithFallbacks(&stubProvider{name: "ecb", rates: map[string]float64{"EUR": 0.92}})
	old := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC) // older than the stub's publication day, so a refetch is due
	w.rawHistory = map[string][]HistoryPoint{"EUR": {{Date: old, RateUSD: 0.93}}}

	w.refreshOnce(context.Background())

	if got := w.rawHistory["EUR"]; len(got) != 1 || got[0].RateUSD != 0.93 {
		t.Fatalf("held history = %+v, want the series kept when the refetch returned nothing", w.rawHistory)
	}
}
