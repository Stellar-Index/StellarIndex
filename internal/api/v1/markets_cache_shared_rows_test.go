// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Findings F014 / K038 — /v1/markets and /v1/pools normalise (and, for
// markets, enrich) their rows IN PLACE. Production wires the handlers to
// a CachedMarketsReader, which used to hand every caller the cache
// entry's own backing array, so:
//
//   - the dex-nonstandard-decimals correction was re-applied on every hit
//     (41.32 → 4132 → 413200 → … — K^N on a served money value), and
//   - concurrent requests wrote the same array elements with no lock.
//
// Every pre-existing listing test wired the handler to a bare stub, so
// the production path (handler → cache) was never exercised. These tests
// wire it the way cmd/stellarindex-api does.

// sharedRowsUpstream stands in for the storage reader BELOW the cache. It
// builds a fresh slice on every call — like the real store and the Redis
// read-through — so any sharing the tests observe is the cache's own.
type sharedRowsUpstream struct {
	calls atomic.Int64
}

const (
	sharedRowsRawFlagged = "41.32"  // raw CAGG ratio, decimals()=9 base leg
	sharedRowsRawClean   = "0.1242" // ordinary 7dp pair
	// K = 10^(9-7) = 100 applied exactly ONCE.
	sharedRowsWantFlagged = "4132.0000000000"
)

func (u *sharedRowsUpstream) markets() []v1.Market {
	u.calls.Add(1)
	flagged, clean := sharedRowsRawFlagged, sharedRowsRawClean
	return []v1.Market{
		{Base: flaggedAsset, Quote: classicUSDC, TradeCount24h: 7, LastPrice: &flagged},
		{Base: "native", Quote: classicUSDC, TradeCount24h: 3, LastPrice: &clean},
	}
}

func (u *sharedRowsUpstream) DistinctPairsExt(context.Context, string, int, timescale.MarketsOrder) ([]v1.Market, string, error) {
	return u.markets(), "", nil
}

func (u *sharedRowsUpstream) SourceMarkets(context.Context, string, string, int, timescale.MarketsOrder) ([]v1.Market, string, error) {
	return u.markets(), "", nil
}

func (u *sharedRowsUpstream) AssetMarkets(context.Context, string, string, int, timescale.MarketsOrder) ([]v1.Market, string, error) {
	return u.markets(), "", nil
}

func (u *sharedRowsUpstream) AllPools(context.Context, timescale.PoolsFilter, string, int, timescale.MarketsOrder) ([]v1.Pool, string, error) {
	u.calls.Add(1)
	flagged, clean := sharedRowsRawFlagged, sharedRowsRawClean
	return []v1.Pool{
		{Source: "aquarius", Base: flaggedAsset, Quote: classicUSDC, LastPrice: &flagged},
		{Source: "aquarius", Base: "native", Quote: classicUSDC, LastPrice: &clean},
	}, "", nil
}

func (u *sharedRowsUpstream) PairMarket(context.Context, canonical.Asset, canonical.Asset) (v1.Market, bool, error) {
	return v1.Market{}, false, nil
}

func (u *sharedRowsUpstream) GetPairsVolumeHistory24hBatch(_ context.Context, pairs [][2]string) (map[string][]timescale.PairVolumePoint, error) {
	out := make(map[string][]timescale.PairVolumePoint, len(pairs))
	for _, p := range pairs {
		out[p[0]+"|"+p[1]] = []timescale.PairVolumePoint{
			{Hour: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), VolumeUSD: "12.5"},
		}
	}
	return out, nil
}

func (u *sharedRowsUpstream) FirstTradeBatch(_ context.Context, pairs [][2]string) (map[string]time.Time, error) {
	out := make(map[string]time.Time, len(pairs))
	for _, p := range pairs {
		out[p[0]+"|"+p[1]] = time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	}
	return out, nil
}

// sharedRowsServer wires handler → CachedMarketsReader → upstream, the
// production shape (cmd/stellarindex-api: NewCachedMarketsReader(…, 2m)).
func sharedRowsServer(t *testing.T) (*testServerImpl, *sharedRowsUpstream) {
	t.Helper()
	up := &sharedRowsUpstream{}
	srv := v1.New(v1.Options{
		Markets:             v1.NewCachedMarketsReader(up, 2*time.Minute),
		NonstandardDecimals: nonstandardDecimalsCacheWith(t, flaggedAsset, 9),
	})
	return startHTTPTest(t, srv.Handler()), up
}

// sharedRowsGet fetches one listing page and returns the raw `data`
// bytes plus the decoded last_price per "base|quote".
func sharedRowsGet(t *testing.T, url string) (string, map[string]string) {
	t.Helper()
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", url, resp.StatusCode)
	}
	body, err := readAll(resp)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", url, err)
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("GET %s: decode envelope: %v (%s)", url, err, body)
	}
	var rows []struct {
		Base      string  `json:"base"`
		Quote     string  `json:"quote"`
		LastPrice *string `json:"last_price"`
	}
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		t.Fatalf("GET %s: decode rows: %v (%s)", url, err, env.Data)
	}
	prices := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.LastPrice != nil {
			prices[r.Base+"|"+r.Quote] = *r.LastPrice
		}
	}
	return string(env.Data), prices
}

func assertSharedRowsPrices(t *testing.T, label string, prices map[string]string) {
	t.Helper()
	if got := prices[flaggedAsset+"|"+classicUSDC]; got != sharedRowsWantFlagged {
		t.Errorf("%s: flagged last_price = %q, want %q (K applied exactly once)", label, got, sharedRowsWantFlagged)
	}
	if got := prices["native|"+classicUSDC]; got != sharedRowsRawClean {
		t.Errorf("%s: 7dp last_price = %q, want byte-identical %q", label, got, sharedRowsRawClean)
	}
}

// TestListings_CachedRows_RepeatHitsServeTheSameLastPrice drives the
// production handler repeatedly against ONE cache entry. Every response
// must equal the first; pre-fix the second hit served 413200 and the
// third 41320000.
func TestListings_CachedRows_RepeatHitsServeTheSameLastPrice(t *testing.T) {
	cases := []struct{ name, path string }{
		{"markets_default", "/v1/markets"},
		{"markets_source", "/v1/markets?source=aquarius"},
		{"markets_asset", "/v1/markets?asset=" + flaggedAsset},
		{"pools", "/v1/pools"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, up := sharedRowsServer(t)
			first, prices := sharedRowsGet(t, ts.URL+tc.path)
			assertSharedRowsPrices(t, "hit 1", prices)
			for hit := 2; hit <= 4; hit++ {
				data, prices := sharedRowsGet(t, ts.URL+tc.path)
				assertSharedRowsPrices(t, fmt.Sprintf("hit %d", hit), prices)
				if data != first {
					t.Errorf("hit %d data differs from hit 1\n first: %s\n   got: %s", hit, first, data)
				}
			}
			// Non-vacuity: hits 2..4 really were served from the one
			// cache entry, not from a fresh upstream read each time.
			if got := up.calls.Load(); got != 1 {
				t.Fatalf("upstream calls = %d, want 1 (repeat hits must come from the cache)", got)
			}
		})
	}
}

// TestListings_CachedRows_ConcurrentHitsAreRaceClean hammers one cache
// entry from many goroutines. Under -race the pre-fix code reports a data
// race on the shared backing array; with or without -race it serves
// compounded prices.
func TestListings_CachedRows_ConcurrentHitsAreRaceClean(t *testing.T) {
	for _, path := range []string{"/v1/markets?include=sparkline,inception", "/v1/pools"} {
		t.Run(path, func(t *testing.T) {
			ts, _ := sharedRowsServer(t)
			const workers, perWorker = 8, 20
			var (
				wg  sync.WaitGroup
				mu  sync.Mutex
				bad []string
			)
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < perWorker; i++ {
						status, body, err := sharedRowsRawGet(ts.URL + path)
						flaggedOK := strings.Contains(body, `"last_price":"`+sharedRowsWantFlagged+`"`)
						cleanOK := strings.Contains(body, `"last_price":"`+sharedRowsRawClean+`"`)
						if err != nil || status != http.StatusOK || !flaggedOK || !cleanOK {
							mu.Lock()
							bad = append(bad, fmt.Sprintf("err %v status %d body %s", err, status, body))
							mu.Unlock()
						}
					}
				}()
			}
			wg.Wait()
			if len(bad) > 0 {
				t.Fatalf("%d of %d concurrent responses were wrong; first: %s", len(bad), workers*perWorker, bad[0])
			}
		})
	}
}

// sharedRowsRawGet is a goroutine-safe GET (no *testing.T — t.Fatal must
// not be called off the test goroutine).
func sharedRowsRawGet(url string) (int, string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	body, err := readAll(resp)
	return resp.StatusCode, body, err
}
