package timescale

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A row without a whole-second window must be refused before anything is
// written: an unattributed row is exactly the collapse GH #763 describes.
// The Store has no database, so reaching the INSERT would panic.
func TestInsertPriceSourceContributions_RefusesAMissingWindow(t *testing.T) {
	s := &Store{}
	valid := PriceSourceContribution{
		AssetID: "crypto:BTC", QuoteID: "fiat:USD", Window: 5 * time.Minute,
		Bucket: time.Now(), Source: "binance", Weight: 1, TradeCount: 1,
	}
	for _, bad := range []time.Duration{0, -time.Minute, 1500 * time.Millisecond} {
		row := valid
		row.Window = bad
		// The invalid row is LAST: validation must cover the whole batch
		// before the first INSERT, not fail midway through it.
		err := s.InsertPriceSourceContributions(context.Background(), []PriceSourceContribution{valid, row})
		if !errors.Is(err, ErrContributionWindowRequired) {
			t.Errorf("Window=%s: err = %v, want ErrContributionWindowRequired", bad, err)
		}
	}
}
