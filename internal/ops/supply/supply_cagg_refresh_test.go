package supply

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A corrective re-derive older than supply_1d's 7-day policy window must
// be refreshed explicitly, or the market-cap chart serves the
// uncorrected day forever (GH #991).
func TestSupplyCAGGRefreshWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }

	observed := time.Date(2026, 8, 26, 17, 30, 0, 0, time.UTC) // 30 days back
	from, to, ok := supplyCAGGRefreshWindow(observed, now)
	if !ok {
		t.Fatal("30-day-old snapshot: ok = false, want a refresh")
	}
	wantFrom := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	if !from.Equal(wantFrom) || !to.Equal(wantTo) {
		t.Errorf("window = [%s, %s), want [%s, %s)", from, to, wantFrom, wantTo)
	}
	if from.After(observed) || !to.After(observed) {
		t.Errorf("window [%s, %s) does not cover %s", from, to, observed)
	}
	if to.Sub(from) < timescale.SupplyCAGG.MinWindow {
		t.Errorf("window %s narrower than MinWindow %s", to.Sub(from), timescale.SupplyCAGG.MinWindow)
	}
	if !timescale.IsRefreshableCAGG(timescale.SupplyCAGG.Name) {
		t.Errorf("%s is not refreshable; the refresh would be refused", timescale.SupplyCAGG.Name)
	}

	// Straddling the policy's 7-day start: refreshed here, not left to rounding.
	if _, _, ok := supplyCAGGRefreshWindow(day(18).Add(3*time.Hour), now); !ok {
		t.Error("7-day-old snapshot: ok = false, want a refresh")
	}
	// Inside the policy window: the 6-hourly policy picks it up.
	for _, obs := range []time.Time{now.Add(-time.Hour), day(20)} {
		if _, _, ok := supplyCAGGRefreshWindow(obs, now); ok {
			t.Errorf("snapshot at %s: ok = true, want the policy to cover it", obs)
		}
	}
}
