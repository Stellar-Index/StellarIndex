// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// dustGuardUSDCIssuer is the embedded catalogue's USDC issuer G-strkey — the
// twin lookup filters ListAssetsExt by this exact issuer (lookupCatalogueTwin).
const dustGuardUSDCIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// dustTwinAssets serves a USDC listing twin whose price came from a SINGLE
// venue (source_count=1) with a POSITIVE sub-floor 24h volume ($10) — the
// shape that trips the dust-liquidity guard on the LISTING path — plus a huge
// circulating supply, so an un-guarded cap would assert billions.
type dustTwinAssets struct {
	AssetsReader // nil embedded — only ListAssetsExt + LatestCirculatingSupply are called
}

func (s *dustTwinAssets) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	// Mirror the real SQL: only the exact Issuer filter returns the twin row.
	if opts.Issuer != dustGuardUSDCIssuer {
		return nil, nil
	}
	price, vol, sourceCount := "0.50", "10", 1
	return []timescale.AssetRow{{
		AssetID:      "USDC-" + dustGuardUSDCIssuer,
		Code:         "USDC",
		PriceUSD:     &price,
		Volume24hUSD: &vol,
		SourceCount:  &sourceCount, // single venue → guard input
	}}, nil
}

// LatestCirculatingSupply feeds fillMarketCapsFromSupply the precise supply so
// the market-cap fill actually runs (10^17 raw @ 7dp × $0.50 = $5B un-guarded).
func (s *dustTwinAssets) LatestCirculatingSupply(context.Context) (map[string]string, error) {
	return map[string]string{"USDC-" + dustGuardUSDCIssuer: "100000000000000000"}, nil
}

// TestCatalogueTwinDustFlagPropagates is the M4 regression: a verified-currency
// catalogue row whose Stellar twin trips the dust-liquidity guard
// (source_count=1 AND positive sub-floor volume) must surface
// market_cap_low_liquidity on the catalogue-LISTING row — matching the twin's
// classic row / detail page. Before the fix, mergeTwinStats copied the (nil)
// cap but dropped the flag, so the listing row silently showed neither cap nor
// flag. The comment claiming "catalogue twins are verified currencies (never
// dust), so the guard's source-count map is a no-op here" was false: the twin
// row comes from ListAssetsExt, which DOES populate source_count.
//
// Fails red against the un-fixed mergeTwinStats (flag stays false); passes
// green with the flag copied through.
func TestCatalogueTwinDustFlagPropagates(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		assetsReader:          &dustTwinAssets{},
		verifiedCurrencies:    cat,
		minMarketCapVolumeUSD: 1000,
	}
	page := []AssetDetail{{Slug: "usdc", Code: "USDC", AssetID: "usdc"}}

	s.fillCatalogueStatsForPage(context.Background(), page)

	if !page[0].MarketCapLowLiquidity {
		t.Errorf("catalogue-listing row must carry market_cap_low_liquidity=true "+
			"from its dust-guarded twin; got false, row=%+v", page[0])
	}
	if page[0].MarketCapUSD != nil {
		t.Errorf("suppressed cap must stay null on the listing row, got %q", *page[0].MarketCapUSD)
	}
}
