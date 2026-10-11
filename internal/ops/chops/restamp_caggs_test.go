package chops

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestRestampCAGGsAreRefreshable pins the restamp follow-up list to the refresh allow-list. The
// follow-up names every aggregate rooted on `trades`; a name RefreshContinuousAggregate rejects
// can only be re-materialised by hand, and a hand-copied list drifts.
func TestRestampCAGGsAreRefreshable(t *testing.T) {
	if len(xlmBaseRestampCAGGs) == 0 {
		t.Fatal("xlmBaseRestampCAGGs is empty")
	}
	for _, c := range xlmBaseRestampCAGGs {
		if !timescale.IsRefreshableCAGG(c.Name) {
			t.Errorf("restamp follow-up names %s, which RefreshContinuousAggregate's allow-list rejects", c.Name)
		}
	}
	if xlmBaseRestampCAGGs[0].Name != "prices_1m" {
		t.Errorf("prices_1m must lead (twap_1h / twap_1d are built on it); got %s", xlmBaseRestampCAGGs[0].Name)
	}
}
