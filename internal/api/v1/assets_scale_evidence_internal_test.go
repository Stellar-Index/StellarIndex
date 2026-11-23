// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
)

// A contradicted-scale market price is withheld before the global-market reference is applied, on detail and listing alike: the
// row is filled from the whole-token global price and carries no divergence or depeg warning measured off the mis-scaled price,
// nor the withheld price's provenance.
func TestWithheldScalePrice_NoGlobalDivergenceFromIt(t *testing.T) {
	for _, path := range []string{"detail", "listing"} {
		t.Run(path, func(t *testing.T) {
			s := globalMarketServer(t, refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "100000000", 8, time.Now()))
			misScaled, pill := "10.00", "0.40" // the market ratio normalised one power of ten off
			asOf := WireTime(time.Now())
			d := AssetDetail{
				AssetID: globalTestUSDC, PriceUSD: &misScaled, Change24hPct: &pill, MarketCapDecimalsMismatch: true,
				PriceSources: []string{"soroswap"}, PriceAsOf: &asOf,
			}
			if path == "detail" {
				s.fillDeclaredPegPrice(context.Background(), &d, nil)
			} else {
				rows := []AssetDetail{d}
				s.fillDeclaredPegPricesInListing(context.Background(), rows)
				d = rows[0]
			}
			if strPtr(d.PriceUSD) != "1.00000000" || d.PriceBasis != priceBasisGlobalMarket {
				t.Fatalf("price_usd = %s basis=%q, want the whole-token global price", strPtr(d.PriceUSD), d.PriceBasis)
			}
			gm := d.GlobalMarket
			if gm == nil || gm.StellarDivergencePct != nil || gm.DepegWarning || gm.IssuerSignals != nil {
				t.Fatalf("global_market = %+v, want no divergence or depeg warning from the withheld price", gm)
			}
			if d.Change24hPct != nil || d.PriceSources != nil || d.PriceAsOf != nil {
				t.Errorf("withheld price's pill or provenance survived the global fill: pill=%v sources=%v as_of=%v",
					d.Change24hPct, d.PriceSources, d.PriceAsOf)
			}
		})
	}
}

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
			asOf := WireTime(time.Now())
			rows := []AssetDetail{{
				AssetID: id, Type: "soroban", Decimals: 7, PriceUSD: &price, Change24hPct: &pill,
				PriceAuthority: aggregate.AuthorityOnChainListing, PriceSources: []string{"soroswap"}, PriceAsOf: &asOf, PriceCredit: "CoinGecko",
			}}
			s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})
			s.fillDeclaredPegPricesInListing(context.Background(), rows)
			r := rows[0]
			if got := r.PriceUSD != nil; got != tc.wantPrice {
				t.Fatalf("price served = %v, want %v", got, tc.wantPrice)
			}
			provenance := r.PriceAuthority != "" || r.PriceSources != nil || r.PriceAsOf != nil || r.PriceCredit != ""
			if tc.wantPrice {
				if r.PriceWithheldReason != "" || r.Change24hPct == nil || !provenance {
					t.Errorf("served row: reason=%q pill=%v provenance=%v, want no reason, the pill and provenance kept",
						r.PriceWithheldReason, r.Change24hPct, provenance)
				}
				return
			}
			if r.PriceWithheldReason != PriceWithheldDecimalsMismatch || r.Change24hPct != nil || r.MarketCapUSD != nil || r.FDVUSD != nil {
				t.Errorf("withheld row: reason=%q pill=%v cap=%v fdv=%v, want decimals_mismatch and every derived figure null",
					r.PriceWithheldReason, r.Change24hPct, r.MarketCapUSD, r.FDVUSD)
			}
			if provenance {
				t.Errorf("withheld row keeps provenance: authority=%q sources=%v as_of=%v credit=%q",
					r.PriceAuthority, r.PriceSources, r.PriceAsOf, r.PriceCredit)
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
