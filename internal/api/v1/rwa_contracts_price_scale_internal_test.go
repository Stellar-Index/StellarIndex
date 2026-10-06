// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A contract row's DEX price comes from asset_price_snapshot, normalised
// through the nonstandard-decimals projection (its confirmed value, else
// 7). The cap may only divide the supply by the scale that price is on.
// 1,074,000 whole tokens of a 6-decimal contract with NO projection row
// carry the raw ratio: supply / 10^6 x that price is 1074000.00, ten
// times the true 107400.00. The lake's 6 still goes on the wire.
func TestRWAContractListingRows_CapDividesByThePriceScale(t *testing.T) {
	const sorobanContract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	const supplyUnits = "1074000000000"
	tests := []struct {
		name         string
		lake         uint32
		projection   map[string]int
		wantCap      string // "" = refused
		wantMismatch bool
	}{
		{"lake 6, no projection row: refused", 6, map[string]int{}, "", true},
		{"lake 6, projection disagrees: refused", 6, map[string]int{sorobanContract: 9}, "", true},
		{"lake 6, projection agrees: published", 6, map[string]int{sorobanContract: 6}, "1074000.00", false},
		{"lake 7, no projection row: published", 7, map[string]int{}, "107400.00", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			price, volume := "1", "100000000.00"
			sources := 3
			row := timescale.AssetRow{AssetID: sorobanContract, PriceUSD: &price, Volume24hUSD: &volume, SourceCount: &sources}
			s := &Server{
				Options: Options{ContractCatalogue: capDecimalsContractCatalogue{rows: map[string]timescale.AssetRow{sorobanContract: row}}, TokenSupply: capDecimalsTokenSupply{byID: map[string]string{sorobanContract: supplyUnits}}, TokenDecimals: fixedTokenDecimals(tc.lake), NonstandardDecimals: decimalsCacheFlagging(t, tc.projection)},
				logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			out, _, cut, err := s.rwaContractListingRows(context.Background(), []rwaContractMember{{contractID: sorobanContract}})
			if err != nil || cut {
				t.Fatalf("cut=%v err=%v", cut, err)
			}
			got := out[sorobanContract]
			gotCap := ""
			if got.MarketCapUSD != nil {
				gotCap = *got.MarketCapUSD
			}
			if gotCap != tc.wantCap {
				t.Errorf("market_cap_usd = %q, want %q", gotCap, tc.wantCap)
			}
			if got.MarketCapDecimalsMismatch != tc.wantMismatch {
				t.Errorf("market_cap_decimals_mismatch = %v, want %v", got.MarketCapDecimalsMismatch, tc.wantMismatch)
			}
			if got.Decimals != int(tc.lake) {
				t.Errorf("decimals = %d, want the lake's %d on the wire", got.Decimals, tc.lake)
			}
			if got.CirculatingSupply == nil || *got.CirculatingSupply != supplyUnits {
				t.Errorf("circulating_supply = %v, want the raw %s served either way", got.CirculatingSupply, supplyUnits)
			}
			wantStatus := RWAValuationPublished
			if tc.wantMismatch {
				wantStatus = RWAValuationDecimalsUnknown
			}
			if v := rwaValuationOf(got); v.Status != wantStatus {
				t.Errorf("valuation status = %q, want %q", v.Status, wantStatus)
			}
		})
	}
}
