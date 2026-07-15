// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// listingOnlyAssets serves change fields ONLY via ListAssetsExt — the
// production shape (the per-asset reader's row carries nil changes),
// which the 2026-07-03 live debug proved after the first enrichment
// deploy merged nothing.
type listingOnlyAssets struct {
	AssetsReader
}

func (s *listingOnlyAssets) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	// Mimic the REAL SQL semantics that broke the first two attempts:
	// Q is a substring match over code/slug/issuer COLUMN VALUES — a
	// full asset id can never match. Only the exact Issuer filter
	// returns rows.
	if opts.Issuer != "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" {
		return nil, nil
	}
	ch, vol := "1.23", "42.00"
	return []timescale.AssetRow{{
		AssetID:      "USDC-" + opts.Issuer,
		Code:         "USDC",
		Change24hPct: &ch,
		Volume24hUSD: &vol,
	}}, nil
}

func (s *listingOnlyAssets) GetAssetByAssetID(_ context.Context, assetID string) (timescale.AssetRow, error) {
	return timescale.AssetRow{AssetID: assetID}, nil // nil changes — production shape
}

// TestCatalogueStatsUseListingReader pins the AM-10 enrichment to the
// listing reader: catalogue rows must gain the twin's change/volume.
func TestCatalogueStatsUseListingReader(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{assetsReader: &listingOnlyAssets{}, verifiedCurrencies: cat}
	page := []AssetDetail{{Slug: "usdc", Code: "USDC", AssetID: "usdc"}}
	req := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
	s.fillCatalogueStatsForPage(req.Context(), page)
	if page[0].Change24hPct == nil || *page[0].Change24hPct != "1.23" {
		t.Fatalf("catalogue row did not absorb the twin's change: %+v", page[0].Change24hPct)
	}
	if page[0].VolumeUSD24h == nil {
		t.Fatal("volume not merged")
	}
}
