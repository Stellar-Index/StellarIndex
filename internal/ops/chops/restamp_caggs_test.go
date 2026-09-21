package chops

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestRestampCAGGsAreRefreshable pins the restamp follow-up list to the
// refresh allow-list. The follow-up names every aggregate rooted on
// `trades`; a name RefreshContinuousAggregate rejects is one an operator
// can only re-materialise by hand, and the hand-copied list drifted to
// five of those (dex_volume_by_pair_1d, source_volume_1h,
// pools_per_source_1h, twap_1h, twap_1d).
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
