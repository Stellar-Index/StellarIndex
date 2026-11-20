// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

type capSupplyStub struct{ snap supply.Supply }

func (s capSupplyStub) LatestSupply(context.Context, string) (supply.Supply, error) {
	return s.snap, nil
}

func (capSupplyStub) DailyCirculatingSupply(context.Context, string, time.Time, time.Time) ([]timescale.SupplyDayPoint, error) {
	return nil, nil
}

// TestMarketCap_PriceFilledAfterF2IsValuedUnderTheSameGuards — a price only the extension step finds (transitive here) gets
// the market cap F2 could not compute, and the dust floor and turnover ceiling still refuse it.
func TestMarketCap_PriceFilledAfterF2IsValuedUnderTheSameGuards(t *testing.T) {
	const (
		assetID = "BLND-GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"
		hopID   = "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN"
	)
	asset, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatal(err)
	}
	vol := func(v string) *string { return &v }
	for _, tc := range []struct {
		name      string
		volume    *string
		floor     float64
		wantCap   string
		wantLowLq bool
	}{
		{"no turnover reading", nil, 0, "767000.00", false},
		{"cap within the turnover ceiling", vol("10000"), 1000, "767000.00", false},
		{"cap above the turnover ceiling", vol("100"), 0, "", true},
		{"single-venue sub-floor volume", vol("100"), 1000, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{
				Options: Options{
					Supply: capSupplyStub{snap: supply.Supply{CirculatingSupply: big.NewInt(100_000_000_000_000), ObservedAt: time.Now()}},
					TransitivePricer: &stubPricer{ok: true, tp: timescale.TransitivePrice{
						PriceUSD: "0.0767", Hop: hopID, HopVolume24hUSD: "5000", HopQuote: "native",
					}},
					Substance:               &stubListingGate{allow: map[string]bool{assetID + "|" + hopID: true, hopID + "|native": true}},
					MaxMarketCapVolumeRatio: 100,
					MinMarketCapVolumeUSD:   tc.floor,
				},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			detail := AssetDetail{Decimals: 7, VolumeUSD24h: tc.volume}
			s.applyF2Fields(context.Background(), &detail, asset)
			s.applyAssetRowToDetail(context.Background(), &detail, asset, timescale.AssetRow{}, sql.ErrNoRows, assetID)
			if detail.PriceUSD == nil || detail.PriceBasis != priceBasisTransitive {
				t.Fatalf("price = %v basis = %q, want the transitive fill", detail.PriceUSD, detail.PriceBasis)
			}
			if got := strOrEmpty(detail.MarketCapUSD); got != tc.wantCap || detail.MarketCapLowLiquidity != tc.wantLowLq {
				t.Errorf("market_cap_usd = %q low_liquidity = %v, want %q %v", got, detail.MarketCapLowLiquidity, tc.wantCap, tc.wantLowLq)
			}
		})
	}
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
