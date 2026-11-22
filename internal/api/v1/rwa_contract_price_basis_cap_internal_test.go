// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import "testing"

// /v1/assets values no cap on a thin-market, ticker-collision, declared-peg or global-market price; the contract arm must refuse
// the same rows with the same reason, and the totals must say they left them out.
func TestContractArmRefusesACapOnItsPriceBasis(t *testing.T) {
	s := &Server{
		Options: Options{TokenSupply: capDecimalsTokenSupply{byID: map[string]string{capRefusalContract: "10000000000"}}},
		logger:  discardLogger(),
	}
	base := func() AssetDetail {
		price, volume := "1.00", "250000"
		return AssetDetail{AssetID: capRefusalContract, Kind: "soroban_token", Decimals: 7, PriceUSD: &price, VolumeUSD24h: &volume}
	}
	clean := rwaValuationOf(capRefusalArm(s, base(), true, 3))
	if clean.Status != RWAValuationPublished || clean.MarketCapUSD == nil || *clean.MarketCapUSD != "1000.00" {
		t.Fatalf("clean row = %+v, want published at 1000.00 (fixture no longer reaches the cap)", clean)
	}
	assets := []RWAAsset{{Valuation: clean}}
	for _, tc := range []struct {
		name   string
		mutate func(*AssetDetail)
		want   MarketCapWithheldReason
	}{
		{"thin market", func(d *AssetDetail) { d.ThinMarket = true }, MarketCapWithheldThinMarket},
		{"ticker collision", func(d *AssetDetail) { d.UnverifiedTickerCollision = true }, MarketCapWithheldTickerCollision},
		{"declared peg", func(d *AssetDetail) { d.PriceBasis = priceBasisDeclaredPeg }, MarketCapWithheldPriceBasis},
		{"global market", func(d *AssetDetail) { d.PriceBasis = priceBasisGlobalMarket }, MarketCapWithheldPriceBasis},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := base()
			tc.mutate(&row)
			v := rwaValuationOf(capRefusalArm(s, row, true, 3))
			if v.MarketCapUSD != nil || v.Status != RWAValuationCapWithheld || v.MarketCapWithheldReason != tc.want {
				t.Errorf("valuation = %q cap %v reason %q, want %q, no cap, reason %q",
					v.Status, ptrStr(v.MarketCapUSD), v.MarketCapWithheldReason, RWAValuationCapWithheld, tc.want)
			}
			assertRWAReasonMatchesStatus(t, v)
			assets = append(assets, RWAAsset{Valuation: v})
		})
	}
	sum := rwaSummarise(assets, rwaTruncation{})
	if sum.AssetsValued != 1 || sum.AssetsUnvalued != 4 || !sum.LowerBound || ptrStr(sum.MarketCapUSD) != "1000.00" {
		t.Errorf("summary valued=%d unvalued=%d lower_bound=%v total=%s, want 1, 4, a lower bound, 1000.00",
			sum.AssetsValued, sum.AssetsUnvalued, sum.LowerBound, ptrStr(sum.MarketCapUSD))
	}
	if capBound, _ := rwaGroupLowerBounds(assets, 1, 0, false); !capBound {
		t.Error("group total over one valued member of five is not flagged a lower bound")
	}
}
