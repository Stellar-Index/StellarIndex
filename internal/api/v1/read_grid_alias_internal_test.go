package v1

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestVWAPBucketBoundsOnTimeBucketGrid: /v1/vwap?breakdown=source buckets on the grid /v1/ohlc bars use (time_bucket's
// 2000-01-03 origin; calendar months for 1mo), so a breakdown bucket explains exactly one bar for every interval.
func TestVWAPBucketBoundsOnTimeBucketGrid(t *testing.T) {
	at := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	for _, r := range timescale.OHLCRoutes {
		iv := ohlcInterval(r.Interval)
		if iv == ohlcInterval1mo {
			continue
		}
		d := iv.duration()
		want := truncateToBucketOrigin(at, d)
		if s, e := vwapBucketBounds(at, iv, time.Time{}, time.Time{}); !s.Equal(want) || !e.Equal(want.Add(d)) {
			t.Errorf("%s: bucket [%s, %s), want [%s, %s)", r.Interval, s, e, want, want.Add(d))
		}
	}
}

// TestLatestOracleReadingsQuoteFilterLoopsAliases: a quote filter given in one spelling keeps rows stored under another
// spelling of the same asset, and still drops a different asset.
func TestLatestOracleReadingsQuoteFilterLoopsAliases(t *testing.T) {
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usd := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}
	now := time.Now().UTC()
	updates := []canonical.OracleUpdate{
		{Source: "reflector", Asset: usd, Quote: xlm, Price: canonical.NewAmount(big.NewInt(1)), Timestamp: now},
		{Source: "reflector", Asset: xlm, Quote: usd, Price: canonical.NewAmount(big.NewInt(1)), Timestamp: now},
	}
	if got := latestOracleReadings(updates, true, canonical.NativeAsset()); len(got) != 1 {
		t.Fatalf("quote=native kept %d rows, want the one quoted in crypto:XLM", len(got))
	}
}
