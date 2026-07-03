// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/StellarIndex/stellar-index/internal/currency"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// listingOnlyCoins serves change fields ONLY via ListCoinsExt — the
// production shape (the per-asset reader's row carries nil changes),
// which the 2026-07-03 live debug proved after the first enrichment
// deploy merged nothing.
type listingOnlyCoins struct {
	CoinsReader
}

func (s *listingOnlyCoins) ListCoinsExt(_ context.Context, opts timescale.ListCoinsOptions) ([]timescale.CoinRow, error) {
	ch, vol := "1.23", "42.00"
	return []timescale.CoinRow{{AssetID: opts.Q, Code: "USDC", Change24hPct: &ch, Volume24hUSD: &vol}}, nil
}

func (s *listingOnlyCoins) GetCoinByAssetID(_ context.Context, assetID string) (timescale.CoinRow, error) {
	return timescale.CoinRow{AssetID: assetID}, nil // nil changes — production shape
}

// TestCatalogueStatsUseListingReader pins the AM-10 enrichment to the
// listing reader: catalogue rows must gain the twin's change/volume.
func TestCatalogueStatsUseListingReader(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{coins: &listingOnlyCoins{}, verifiedCurrencies: cat}
	page := []AssetDetail{{Slug: "usdc", Code: "USDC", AssetID: "usdc"}}
	req := httptest.NewRequest("GET", "/v1/assets", nil)
	s.fillCatalogueStatsForPage(req.Context(), page)
	if page[0].Change24hPct == nil || *page[0].Change24hPct != "1.23" {
		t.Fatalf("catalogue row did not absorb the twin's change: %+v", page[0].Change24hPct)
	}
	if page[0].VolumeUSD24h == nil {
		t.Fatal("volume not merged")
	}
}
