// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// GH-1009: /v1/assets never resolved a Soroban row's decimals() on the
// LISTING page — only the confirmed nonstandard_decimals_assets projection,
// which lags a freshly-observed or not-yet-DEX-traded token. A listing row
// kept the default 7 while the SAME asset's detail page read the lake
// directly and got the true scale, publishing a market cap 10^(7-decimals)x
// off between the two surfaces in the same second.
//
// listingLockstepDecStub is the internal-package counterpart of the
// v1_test package's lockstepDecStub (unexported TokenDecimalsReader,
// unreachable from here).
type listingLockstepDecStub struct {
	d     uint32
	found bool
}

func (s *listingLockstepDecStub) TokenDecimals(_ context.Context, _ string) (uint32, bool, error) {
	return s.d, s.found, nil
}

// listingLockstepConfirmedReader is a package-local NonstandardDecimalsReader
// stub (the v1_test package's stubNonstandardDecimalsReader is unreachable
// from here).
type listingLockstepConfirmedReader struct {
	rows []timescale.NonstandardDecimalsAsset
}

func (r *listingLockstepConfirmedReader) LoadNonstandardDecimalsAssets(_ context.Context) ([]timescale.NonstandardDecimalsAsset, error) {
	return r.rows, nil
}

// TestFillRowMarketCap_SorobanLockstep_LakeNonstandardProjectionUnseeded is
// PROVEN RED on the unfixed fillRowMarketCap/applyConfirmedListingDecimals,
// which consulted ONLY the confirmed projection (nil here — the guard
// hasn't seeded this contract yet) and left row.Decimals at the
// assetDetailFromAssetRow default of 7. 1000 tokens at 9dp (raw
// 1000000000000) times the raw 41.32 ratio at the wrong divisor published
// market_cap_usd="41320.00" — a hundredth of the true 41,320,000,000,000.00
// scale — instead of refusing.
func TestFillRowMarketCap_SorobanLockstep_LakeNonstandardProjectionUnseeded(t *testing.T) {
	const contractID = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
	s := &Server{tokenDecimals: &listingLockstepDecStub{d: 9, found: true}}
	price := "41.32"
	row := AssetDetail{
		AssetID:  contractID,
		Type:     string(canonical.AssetSoroban),
		Code:     "TKN",
		Decimals: 7, // assetDetailFromAssetRow's protocol default
		PriceUSD: &price,
	}
	precise := observedSupply(map[string]string{contractID: "1000000000000"})

	s.fillRowMarketCap(context.Background(), &row, precise, nil, nil, map[string]int{contractID: 5})

	if row.MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %q, want refused — the lake's live decimals() (9) and the "+
			"unseeded projection default (7) disagree, so supply and price are on different scales",
			*row.MarketCapUSD)
	}
	if !row.MarketCapDecimalsMismatch {
		t.Error("market_cap_decimals_mismatch must be set — the listing page must refuse a cap it can't scale correctly, matching the detail page's lockstep guard")
	}
	if row.Decimals != 9 {
		t.Errorf("decimals = %d, want 9 (the live lake reading, not the unseeded default)", row.Decimals)
	}
	if row.CirculatingSupply == nil {
		t.Error("circulating_supply is a raw fact and must still serve despite the refused cap")
	}
}

// TestFillRowMarketCap_SorobanLockstep_Agreement is the control: a
// confirmed projection row that AGREES with the lake serves the cap as
// before, on the corrected (non-default) scale.
func TestFillRowMarketCap_SorobanLockstep_Agreement(t *testing.T) {
	const contractID = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
	reader := &listingLockstepConfirmedReader{
		rows: []timescale.NonstandardDecimalsAsset{{Asset: contractID, Decimals: 9}},
	}
	cache := NewNonstandardDecimalsCache(reader, nil)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	s := &Server{
		tokenDecimals:       &listingLockstepDecStub{d: 9, found: true},
		nonstandardDecimals: cache,
	}
	price := "41.32"
	row := AssetDetail{
		AssetID:  contractID,
		Type:     string(canonical.AssetSoroban),
		Code:     "TKN",
		Decimals: 7,
		PriceUSD: &price,
	}
	precise := observedSupply(map[string]string{contractID: "1000000000000"})

	s.fillRowMarketCap(context.Background(), &row, precise, nil, nil, map[string]int{contractID: 5})

	if row.MarketCapDecimalsMismatch {
		t.Error("market_cap_decimals_mismatch must be false when the lake and the confirmed projection agree")
	}
	if row.MarketCapUSD == nil {
		t.Fatal("market_cap_usd must serve when both decimals resolvers agree")
	}
	if row.Decimals != 9 {
		t.Errorf("decimals = %d, want 9", row.Decimals)
	}
}
