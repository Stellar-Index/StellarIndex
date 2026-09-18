// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A listing row's price_usd comes from asset_price_snapshot, whose writer
// stores the TRUE-scale price for a confirmed non-7-decimals token
// (timescale's snapshotNormalizedPriceUSDExpr). The market cap multiplies
// that price by a supply held in the token's SMALLEST unit, so the supply
// divisor is a consumer of the price column's scale: it has to be the
// token's real decimals, not the 7 assetDetailFromAssetRow stamps on
// every row.
//
// While the snapshot stored the RAW ratio the two errors cancelled —
// supply / 10^7 times a price 10^(7 - decimals) off is the right number —
// which is how moving the price to true scale turned a correct cap into
// one wrong by 10^(decimals - 7): 1,000 tokens of a 9-decimals contract
// at 2.50 USD published 250000.00 instead of 2500.00.
func TestFillMarketCapsFromSupply_DividesSupplyByTheConfirmedDecimals(t *testing.T) {
	const sorobanContract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	tests := []struct {
		name         string
		flagged      map[string]int
		supply       string // smallest units; 1,000 whole tokens in every row
		price        string // TRUE-scale, as asset_price_snapshot stores it
		wantCap      string
		wantDecimals int
	}{
		{"9dp", map[string]int{sorobanContract: 9}, "1000000000000", "2.5000000000", "2500.00", 9},
		{"18dp", map[string]int{sorobanContract: 18}, "1000000000000000000000", "14.0000000000", "14000.00", 18},
		{"5dp", map[string]int{sorobanContract: 5}, "100000000", "2.0000000000", "2000.00", 5},
		// No confirmed row: the price is the raw ratio it always was and
		// the standard divisor is the one that matches it. Unchanged.
		{"unflagged keeps the standard 7", map[string]int{}, "10000000000", "2.5000000000", "2500.00", 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			price := tc.price
			rows := []AssetDetail{assetDetailFromAssetRow(timescale.AssetRow{AssetID: sorobanContract, PriceUSD: &price})}
			s := &Server{
				assetsReader: &preciseSupplyStub{obs: map[string]timescale.SupplyObservation{
					sorobanContract: {CirculatingSupply: tc.supply, Basis: "sep41_lake_flows"},
				}},
				nonstandardDecimals: decimalsCacheFlagging(t, tc.flagged),
			}
			s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})

			got := rows[0]
			if got.MarketCapUSD == nil {
				t.Fatalf("market_cap_usd = nil, want %s", tc.wantCap)
			}
			if *got.MarketCapUSD != tc.wantCap {
				t.Errorf("market_cap_usd = %s, want %s (supply / 10^%d x price)", *got.MarketCapUSD, tc.wantCap, tc.wantDecimals)
			}
			// The published divisor is the one the cap was computed with,
			// so a client redoing the arithmetic gets the same figure.
			if got.Decimals != tc.wantDecimals {
				t.Errorf("decimals = %d, want %d", got.Decimals, tc.wantDecimals)
			}
			if got.CirculatingSupply == nil || *got.CirculatingSupply != tc.supply {
				t.Errorf("circulating_supply = %v, want the raw reading %s untouched", got.CirculatingSupply, tc.supply)
			}
			if got.PriceUSD == nil || *got.PriceUSD != tc.price {
				t.Errorf("price_usd = %v, want %s untouched — the writer already normalised it", got.PriceUSD, tc.price)
			}
		})
	}

	// No cache wired at all (a test server, a degraded boot): nothing is
	// on record, so nothing changes.
	price := "2.5000000000"
	rows := []AssetDetail{assetDetailFromAssetRow(timescale.AssetRow{AssetID: sorobanContract, PriceUSD: &price})}
	s := &Server{assetsReader: &preciseSupplyStub{obs: map[string]timescale.SupplyObservation{
		sorobanContract: {CirculatingSupply: "10000000000", Basis: "sep41_lake_flows"},
	}}}
	s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})
	if rows[0].MarketCapUSD == nil || *rows[0].MarketCapUSD != "2500.00" || rows[0].Decimals != 7 {
		t.Errorf("nil cache: cap = %v decimals = %d, want 2500.00 at 7", rows[0].MarketCapUSD, rows[0].Decimals)
	}
}

type capDecimalsContractCatalogue struct{ rows map[string]timescale.AssetRow }

func (c capDecimalsContractCatalogue) ContractCatalogueRows(
	context.Context, []string,
) (map[string]timescale.AssetRow, error) {
	return c.rows, nil
}

type capDecimalsTokenSupply struct{ byID map[string]string }

func (c capDecimalsTokenSupply) TokenSupply(_ context.Context, id string) (clickhouse.TokenSupply, error) {
	n, ok := new(big.Int).SetString(c.byID[id], 10)
	if !ok {
		return clickhouse.TokenSupply{ContractID: id}, nil
	}
	return clickhouse.TokenSupply{ContractID: id, Total: n, FlowCount: 1}, nil
}

func (capDecimalsTokenSupply) NativeTotalCoins(context.Context) (int64, uint32, error) {
	return 0, 0, errors.New("the contract arm must not read the native total")
}

// The RWA contract arm runs the shared listing fill BEFORE it has read
// the contract's decimals, then fills its own cap afterwards — and every
// `continue` in that second fill leaves whatever the first one published
// standing. The reachable case: the supply observer covers the contract,
// the decimals reader cannot answer (it is a separate ClickHouse dial),
// so the row is marked decimals-unresolved and keeps the first fill's
// cap. That cap may not be the one computed with the hardcoded 7.
func TestRWAContractListingRows_EarlyCapIsNotOnTheHardcodedSevenScale(t *testing.T) {
	const sorobanContract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	price, volume := "2.5000000000", "250000.00"
	sources := 3
	row := timescale.AssetRow{AssetID: sorobanContract, PriceUSD: &price, Volume24hUSD: &volume, SourceCount: &sources}

	s := &Server{
		assetsReader: &preciseSupplyStub{obs: map[string]timescale.SupplyObservation{
			sorobanContract: {CirculatingSupply: "1000000000000", Basis: "sep41_lake_flows"},
		}},
		contractCatalogue:   capDecimalsContractCatalogue{rows: map[string]timescale.AssetRow{sorobanContract: row}},
		tokenSupply:         capDecimalsTokenSupply{byID: map[string]string{sorobanContract: "1000000000000"}},
		nonstandardDecimals: decimalsCacheFlagging(t, map[string]int{sorobanContract: 9}),
		// tokenDecimals deliberately unwired: the scale is never READ.
	}
	out, _, err := s.rwaContractListingRows(context.Background(), []rwaContractMember{{contractID: sorobanContract}})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := out[sorobanContract]
	if !ok {
		t.Fatal("contract row missing from the listing")
	}
	if got.MarketCapUSD != nil && *got.MarketCapUSD != "2500.00" {
		t.Errorf("market_cap_usd = %s, want 2500.00 or none — 250000.00 is supply / 10^7 x a true-scale price", *got.MarketCapUSD)
	}
}
