package main

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/orchestrator"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The orchestrator records one breakdown per (pair, window). Each window's
// rows must carry that window, or the 5m, 1h and 24h breakdowns of one pair
// land in price_source_contributions indistinguishable (GH #763).
func TestContributionRows_CarryTheRecordsWindow(t *testing.T) {
	base, err := canonical.NewCryptoAsset("BTC")
	if err != nil {
		t.Fatal(err)
	}
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	computedAt := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	for _, window := range []time.Duration{5 * time.Minute, time.Hour, 24 * time.Hour} {
		rows := contributionRows(orchestrator.ContributionRecord{
			Pair:       canonical.Pair{Base: base, Quote: quote},
			Window:     window,
			ComputedAt: computedAt,
			Contributions: []aggregate.SourceContribution{
				{Source: "binance", Weight: 0.6, TradeCount: 3},
				{Source: "kraken", Weight: 0.4, TradeCount: 2},
			},
			SourceUSDVolume: map[string]float64{"binance": 1200},
		})
		if len(rows) != 2 {
			t.Fatalf("window %s: got %d rows, want 2", window, len(rows))
		}
		for _, r := range rows {
			if r.Window != window {
				t.Errorf("window %s: row for %s carries Window=%s, want %s", window, r.Source, r.Window, window)
			}
			if !r.Bucket.Equal(computedAt) || r.AssetID != base.String() || r.QuoteID != quote.String() {
				t.Errorf("window %s: row %+v lost its pair or bucket", window, r)
			}
		}
		if rows[0].VolumeUSD == nil || *rows[0].VolumeUSD != 1200 || rows[1].VolumeUSD != nil {
			t.Errorf("window %s: volume_usd mapping changed: %v / %v", window, rows[0].VolumeUSD, rows[1].VolumeUSD)
		}
	}
}
