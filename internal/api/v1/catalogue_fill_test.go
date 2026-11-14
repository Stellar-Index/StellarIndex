// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// listingOnlyAssets serves change fields ONLY via ListAssetsExt — the
// production shape (the per-asset reader's row carries nil changes),
// which a live debug showed when enrichment merged nothing.
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

// TestCatalogueStatsUseListingReader pins the catalogue enrichment to the
// listing reader: catalogue rows must gain the twin's change/volume.
func TestCatalogueStatsUseListingReader(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Options: Options{AssetsReader: &listingOnlyAssets{}, VerifiedCurrencies: cat}}
	page := []AssetDetail{{Slug: "usdc", Code: "USDC", AssetID: "usdc"}}
	req := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
	s.fillCatalogueStatsForPage(req.Context(), page, assetListFilters{})
	if page[0].Change24hPct == nil || *page[0].Change24hPct != "1.23" {
		t.Fatalf("catalogue row did not absorb the twin's change: %+v", page[0].Change24hPct)
	}
	if page[0].VolumeUSD24h == nil {
		t.Fatal("volume not merged")
	}
}

// failingSupplyAssets is listingOnlyAssets whose supply observer read fails, counted.
type failingSupplyAssets struct {
	listingOnlyAssets
	supplyReads atomic.Int32
}

func (f *failingSupplyAssets) LatestSupplyObservations(context.Context, time.Duration) (map[string]timescale.SupplyObservation, error) {
	f.supplyReads.Add(1)
	return nil, errors.New("pool timeout")
}

// TestCatalogueStatsReadPreciseSupplyOncePerPage: a failing observer read marks the page and is not repeated per entry.
func TestCatalogueStatsReadPreciseSupplyOncePerPage(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	reader := &failingSupplyAssets{}
	s := unreadTestServer(Options{AssetsReader: reader, VerifiedCurrencies: cat})
	page := []AssetDetail{{Slug: "usdc", Code: "USDC", AssetID: "usdc"}, {Slug: "usdc", Code: "USDC", AssetID: "usdc"}}
	if !s.fillCatalogueStatsForPage(context.Background(), page, assetListFilters{}) {
		t.Error("supply read failed: page reported measured")
	}
	if n := reader.supplyReads.Load(); n != 1 {
		t.Errorf("supply observer read %d times for a two-entry page, want 1", n)
	}
}
