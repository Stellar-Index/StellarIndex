// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"
)

// The listing withholds on the same evidence as the detail page, in the same place: after the market producers, before the
// whole-token fills.
func TestListing_PriceWithheldOnlyOnContradictedScale(t *testing.T) {
	id := syntheticContractID(t, 7)
	for _, tc := range []struct {
		name      string
		reader    TokenDecimalsReader
		wantPrice bool
	}{
		{"reader nil", nil, true},
		{"batch failed", &countingScaleReader{batchErr: context.DeadlineExceeded}, true},
		{"read declares nothing", &countingScaleReader{declared: map[string]uint32{}}, true},
		{"read agrees", &countingScaleReader{declared: map[string]uint32{id: 7}}, true},
		{"read contradicts", &countingScaleReader{declared: map[string]uint32{id: 18}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{Options: Options{TokenDecimals: tc.reader}, logger: discardLogger()}
			price, pill := "41.32", "3.10"
			rows := []AssetDetail{{AssetID: id, Type: "soroban", Decimals: 7, PriceUSD: &price, Change24hPct: &pill}}
			s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})
			s.fillDeclaredPegPricesInListing(context.Background(), rows)
			r := rows[0]
			if got := r.PriceUSD != nil; got != tc.wantPrice {
				t.Fatalf("price served = %v, want %v", got, tc.wantPrice)
			}
			if tc.wantPrice {
				if r.PriceWithheldReason != "" || r.Change24hPct == nil {
					t.Errorf("served row: reason=%q pill=%v, want no reason and the pill kept", r.PriceWithheldReason, r.Change24hPct)
				}
				return
			}
			if r.PriceWithheldReason != PriceWithheldDecimalsMismatch || r.Change24hPct != nil || r.MarketCapUSD != nil || r.FDVUSD != nil {
				t.Errorf("withheld row: reason=%q pill=%v cap=%v fdv=%v, want decimals_mismatch and every derived figure null",
					r.PriceWithheldReason, r.Change24hPct, r.MarketCapUSD, r.FDVUSD)
			}
		})
	}
}

// A price quoted per whole token carries no token scale, so a contradicted scale leaves it (and the cap refusal) alone.
func TestWithholdContradictedScalePrice_WholeTokenPricesExempt(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  func(p *string) AssetDetail
	}{
		{"global market", func(p *string) AssetDetail { return AssetDetail{PriceUSD: p, PriceBasis: priceBasisGlobalMarket} }},
		{"declared peg", func(p *string) AssetDetail { return AssetDetail{PriceUSD: p, PriceBasis: priceBasisDeclaredPeg} }},
		{"twin's whole-token row price", func(p *string) AssetDetail { return AssetDetail{PriceUSD: p, priceWholeToken: true} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := "1.00"
			d := tc.row(&p)
			d.MarketCapDecimalsMismatch = true
			withholdContradictedScalePrice(&d)
			if d.PriceUSD == nil || d.PriceWithheldReason != "" {
				t.Errorf("whole-token price withheld: price=%v reason=%q", d.PriceUSD, d.PriceWithheldReason)
			}
		})
	}
}
