package main

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/orchestrator"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The orchestrator records one breakdown per (pair, window). Each window's
// rows must carry that window, or the 5m, 1h and 24h breakdowns of one pair
// land in price_source_contributions indistinguishable.
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
				{Source: "binance", Weight: big.NewRat(3, 5), TradeCount: 3},
				{Source: "kraken", Weight: big.NewRat(2, 5), TradeCount: 2},
			},
			SourceUSDVolume: map[string]*big.Rat{"binance": big.NewRat(1200, 1)},
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
		if rows[0].VolumeUSD == nil || *rows[0].VolumeUSD != "1200.000000000000000000" || rows[1].VolumeUSD != nil {
			t.Errorf("window %s: volume_usd mapping changed: %v / %v", window, rows[0].VolumeUSD, rows[1].VolumeUSD)
		}
	}
}

// Weight and volume_usd land in NUMERIC columns and must reach them at
// decimal precision, not float64's ~16 significant digits.
// A USD volume above 2^53 cents and a 1/3 share both lose digits as float.
func TestContributionRows_RenderExactDecimals(t *testing.T) {
	vol, ok := new(big.Rat).SetString("90071992547409.93") // 2^53+1 cents
	if !ok {
		t.Fatal("parse volume")
	}
	rows := contributionRows(orchestrator.ContributionRecord{
		Window: time.Hour,
		Contributions: []aggregate.SourceContribution{
			{Source: "a", Weight: big.NewRat(1, 3), TradeCount: 1},
			{Source: "b", Weight: big.NewRat(2, 3), TradeCount: 1},
		},
		SourceUSDVolume: map[string]*big.Rat{"a": vol},
	})
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Weight != "0.333333333333333333" || rows[1].Weight != "0.666666666666666667" {
		t.Errorf("weights = %s / %s, want 18-digit decimals of 1/3 and 2/3", rows[0].Weight, rows[1].Weight)
	}
	if rows[0].VolumeUSD == nil || *rows[0].VolumeUSD != "90071992547409.930000000000000000" {
		t.Errorf("volume_usd = %v, want 90071992547409.93 exactly", rows[0].VolumeUSD)
	}
}
