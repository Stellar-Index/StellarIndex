// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A broad map served past its TTL because refresh keeps failing is a stale read, and the listing must say so.
func TestClassicSupplyMap_ExpiredEntryReportsStale(t *testing.T) {
	for name, tc := range map[string]struct {
		age       time.Duration
		wantStale bool
	}{
		"fresh":              {age: time.Minute, wantStale: false},
		"expired, servable":  {age: classicSupplyTTL + time.Minute, wantStale: true},
		"just under max age": {age: classicSupplyMaxAge - time.Minute, wantStale: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := lakeSupplyServer(t, "")
			s.Explorer = failingClassicExplorer{}
			s.classicSupplyCache = map[string]string{cetesAsset: "400000000"}
			s.classicSupplyAt = time.Now().Add(-tc.age)
			s.classicSupplyAttemptAt = time.Now() // retry gap open: no refresh can replace the entry
			ctx, rep := withReadReport(context.Background())
			m, unread := s.classicSupplyMap(ctx)
			if m == nil || unread {
				t.Fatalf("map = %v, unread = %v; want the cached map served", m, unread)
			}
			if got := rep.stale.Load(); got != tc.wantStale {
				t.Errorf("stale = %v, want %v", got, tc.wantStale)
			}
		})
	}
}

func TestFillMarketCapsFromSupply_ExpiredBroadMapFlagsStale(t *testing.T) {
	s, _ := lakeSupplyServer(t, "")
	s.Explorer = failingClassicExplorer{}
	s.classicSupplyCache = map[string]string{cetesAsset: "400000000"}
	s.classicSupplyAt = time.Now().Add(-(classicSupplyTTL + time.Minute))
	s.classicSupplyAttemptAt = time.Now()
	ctx, rep := withReadReport(context.Background())
	s.fillMarketCapsFromSupply(ctx, cetesRow(), map[string]int{})
	if !rep.stale.Load() {
		t.Error("market caps built from an expired broad supply map left the response unflagged")
	}
}

// The production listing caller, not a test-built report, must carry an expired broad map into the page's flags.
func TestAssetListPage_ExpiredBroadMapIsStale(t *testing.T) {
	for name, tc := range map[string]struct {
		age   time.Duration
		stale bool
	}{
		"fresh":   {time.Minute, false},
		"expired": {classicSupplyTTL + time.Minute, true},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := lakeSupplyServer(t, "")
			s.Explorer = failingClassicExplorer{}
			s.classicSupplyCache = map[string]string{cetesAsset: "400000000"}
			s.classicSupplyAt = time.Now().Add(-tc.age)
			s.classicSupplyAttemptAt = time.Now()
			price := "1.00"
			s.AssetsReader = &classicPageReader{rows: []timescale.AssetRow{{
				AssetID: cetesAsset, Code: "CETES", IssuerGStrkey: strings.SplitN(cetesAsset, "-", 2)[1], PriceUSD: &price,
			}}}
			req := httptest.NewRequest(http.MethodGet, "/v1/assets?limit=10", nil)
			env, rows, err := s.buildAssetListPage(req, assetListFilters{}, "", 10, timescale.AssetsOrderVolume24hUSDDesc)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].CirculatingSupply == nil || *rows[0].CirculatingSupply != "400000000" {
				t.Fatalf("instrument: want CETES valued on the cached map, got %+v", rows)
			}
			if env.Flags.Stale != tc.stale || assetListPageCacheable(env, rows) == tc.stale {
				t.Errorf("stale = %v, cacheable = %v; want stale %v", env.Flags.Stale, assetListPageCacheable(env, rows), tc.stale)
			}
		})
	}
}

// A thin-admitted listing row keeps its supply, so its thin_market cap reason survives MarshalJSON, as on the detail route.
func TestFillMarketCapsFromSupply_ThinRowKeepsSupplyAndReason(t *testing.T) {
	s, _ := lakeSupplyServer(t, "")
	s.Explorer = failingClassicExplorer{}
	s.classicSupplyCache = map[string]string{cetesAsset: "400000000"}
	s.classicSupplyAt, s.classicSupplyAttemptAt = time.Now(), time.Now()
	rows := cetesRow()
	rows[0].ThinMarket = true
	s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})
	body, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].MarketCapUSD != nil || !strings.Contains(string(body), `"circulating_supply":"400000000"`) ||
		!strings.Contains(string(body), `"market_cap_withheld_reason":"thin_market"`) {
		t.Errorf("thin row: want supply, no cap and the thin_market reason on the wire: %s", body)
	}
}

// cachedClassicSupply never reports, even for an expired entry: its only caller is the background prewarm, which serves
// nothing. Request paths use classicSupplyMap.
func TestCachedClassicSupply_ExpiredEntryDoesNotReport(t *testing.T) {
	s, _ := lakeSupplyServer(t, "")
	s.Explorer = failingClassicExplorer{}
	s.classicSupplyCache = map[string]string{cetesAsset: "400000000"}
	s.classicSupplyAt = time.Now().Add(-(classicSupplyTTL + time.Minute))
	s.classicSupplyAttemptAt = time.Now()
	ctx, rep := withReadReport(context.Background())
	if m := s.cachedClassicSupply(ctx); m == nil || rep.stale.Load() {
		t.Errorf("map = %v, stale = %v; want the map served and nothing reported", m, rep.stale.Load())
	}
}
