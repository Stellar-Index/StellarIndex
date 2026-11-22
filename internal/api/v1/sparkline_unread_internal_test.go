// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// failingSparklineReader is a listing whose 7d sparkline batch read fails, counted so a test can prove the attach ran.
type failingSparklineReader struct {
	classicPageReader
	calls atomic.Int32
}

func (f *failingSparklineReader) GetAssetsPriceHistory7dBatch(context.Context, []string) (map[string][]timescale.AssetPricePoint, error) {
	f.calls.Add(1)
	return nil, errors.New("prices_1m unreachable")
}

func sparklineFailServer(t *testing.T) (*Server, *failingSparklineReader) {
	t.Helper()
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{VerifiedCurrencies: cat})
	s.logger = discardLogger()
	price := "1.00"
	rd := &failingSparklineReader{classicPageReader: classicPageReader{rows: []timescale.AssetRow{{
		AssetID: cetesAsset, Code: "CETES", IssuerGStrkey: strings.SplitN(cetesAsset, "-", 2)[1], PriceUSD: &price,
	}}}}
	s.AssetsReader = rd
	return s, rd
}

// A requested sparkline whose read failed is missing from a page that otherwise looks whole; the page must not be clean.
func TestListing_FailedSparklineReadDegradesThePage(t *testing.T) {
	const url = "/v1/assets?limit=10&include=sparkline7d"
	t.Run("buildAssetListPage", func(t *testing.T) {
		s, rd := sparklineFailServer(t)
		env, rows, err := s.buildAssetListPage(httptest.NewRequest(http.MethodGet, url, nil), assetListFilters{}, "", 10,
			timescale.AssetsOrderVolume24hUSDDesc)
		if err != nil {
			t.Fatal(err)
		}
		if rd.calls.Load() == 0 {
			t.Fatal("instrument: the sparkline batch was never read")
		}
		if !env.Flags.Degraded || assetListPageCacheable(env, rows) {
			t.Errorf("degraded = %v, cacheable = %v; a failed sparkline read must degrade the page",
				env.Flags.Degraded, assetListPageCacheable(env, rows))
		}
	})
	t.Run("serveClassicUnifiedPage", func(t *testing.T) {
		s, rd := sparklineFailServer(t)
		rec := httptest.NewRecorder()
		s.serveClassicUnifiedPage(rec, httptest.NewRequest(http.MethodGet, url+"&asset_class=all&cursor=classic:", nil),
			assetListFilters{}, 10, "")
		if rd.calls.Load() == 0 {
			t.Fatal("instrument: the sparkline batch was never read")
		}
		if !noStore(rec) {
			t.Errorf("Cache-Control = %q; a failed sparkline read must not be cached as clean", rec.Header().Get("Cache-Control"))
		}
	})
	t.Run("serveCatalogueUnifiedPage", func(t *testing.T) {
		s, rd := sparklineFailServer(t)
		rec := httptest.NewRecorder()
		s.serveCatalogueUnifiedPage(rec, httptest.NewRequest(http.MethodGet, url+"&asset_class=all", nil),
			assetListFilters{}, 100, "")
		if rd.calls.Load() == 0 {
			t.Fatal("instrument: the sparkline batch was never read")
		}
		if !noStore(rec) {
			t.Errorf("Cache-Control = %q; a failed sparkline read must not be cached as clean", rec.Header().Get("Cache-Control"))
		}
	})
}
