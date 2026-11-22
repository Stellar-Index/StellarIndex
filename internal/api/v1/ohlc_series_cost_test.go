// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// The explorer MarketChart's request per timeframe (web/explorer/src/components/charts/MarketChart.tsx: limit =
// ceil(span / interval) + 2); the home page opens on 7d.
const (
	homeChart7d  = "/v1/ohlc?base=native&quote=fiat:USD&interval=15m&limit=674"
	homeChart24h = "/v1/ohlc?base=native&quote=fiat:USD&interval=5m&limit=290"
	homeChart30d = "/v1/ohlc?base=native&quote=fiat:USD&interval=1h&limit=722"
)

func newSeriesCostServer(t *testing.T, budget int) (*testServerImpl, *readCountingHistory) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	reader := &readCountingHistory{}
	ts := startHTTPTest(t, v1.New(v1.Options{
		History: reader,
		RateLimit: middleware.RateLimitBySubject(
			ratelimit.New(rdb, budget, time.Minute, pinnedWindow), nil, middleware.SkipHealthAndMetrics, nil),
	}).Handler())
	return ts, reader
}

// seriesCharge is what one request into a fresh window was charged, read off a probe (a 400 costing the base token).
func seriesCharge(t *testing.T, path string) (charged int, reads int64) {
	t.Helper()
	const budget = 100_000
	ts, reader := newSeriesCostServer(t, budget)
	if resp := mustGet(t, ts.URL+path); resp.StatusCode == http.StatusTooManyRequests {
		t.Fatalf("%s: first request into a fresh window was refused", path)
	}
	left, err := strconv.Atoi(mustGet(t, ts.URL+"/v1/ohlc").Header.Get("X-RateLimit-Remaining"))
	if err != nil {
		t.Fatalf("probe X-RateLimit-Remaining: %v", err)
	}
	return budget - (left + 1), reader.reads.Load()
}

// TestOHLCSeriesCost_ChargesRowsAcrossBooks: a series read is charged for the rows it aggregates across every book, one
// token per chartPointsPerToken, so the explorer's anonymous home chart (re-polled on every ledger) is a few tokens a hit,
// not one per fiat constituent.
func TestOHLCSeriesCost_ChargesRowsAcrossBooks(t *testing.T) {
	cases := []struct {
		path string
		want int
	}{
		{homeChart7d, 3},  // 674 15m rows x 18 books
		{homeChart24h, 6}, // 1450 1m rows (folded to 5m) x 18 books
		{homeChart30d, 3}, // 722 1h rows x 18 books
	}
	for _, tc := range cases {
		charged, reads := seriesCharge(t, tc.path)
		if reads != 18 {
			t.Fatalf("%s: %d book reads, want the 18-book fiat fan-out this table is priced for", tc.path, reads)
		}
		if charged != tc.want {
			t.Errorf("%s over %d books: charged %d tokens, want %d", tc.path, reads, charged, tc.want)
		}
	}

	anon := config.Default().API.AnonRateLimitPerMin
	ts, _ := newSeriesCostServer(t, anon)
	for i, path := range []string{homeChart7d, homeChart7d, homeChart7d, homeChart7d, homeChart24h, homeChart30d} {
		if resp := mustGet(t, ts.URL+path); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d (%s) inside the %d-token anonymous minute: status %d", i, path, anon, resp.StatusCode)
		}
	}
}

// TestOHLCSeriesCost_ScalesWithRowsAndBooks: a year of 1m rows still costs in proportion to the rows, and the fiat fan-out
// multiplies them; one the budget cannot cover is refused before any read.
func TestOHLCSeriesCost_ScalesWithRowsAndBooks(t *testing.T) {
	from := time.Now().UTC().Truncate(time.Minute).Add(-365 * 24 * time.Hour).Format(time.RFC3339)
	oneBook, _ := seriesCharge(t, "/v1/ohlc?base=native&quote=crypto:USDT&interval=1m&from="+from)
	fiat, fiatReads := seriesCharge(t, "/v1/ohlc?base=native&quote=fiat:USD&interval=1m&from="+from)
	if fiatReads < 2 {
		t.Fatalf("fiat series issued %d book reads; the fan-out did not run", fiatReads)
	}
	// 525,600 rows (one more when `to` does not land on a minute) per book at 5,000 rows a token.
	if oneBook < 106 || oneBook > 107 {
		t.Errorf("a year of 1m rows on one book charged %d tokens, want 106", oneBook)
	}
	if rows := 525_600 * int(fiatReads); fiat*5000 < rows {
		t.Errorf("a year of 1m rows over %d fiat books (%d rows) charged %d tokens", fiatReads, rows, fiat)
	}

	// The first read spends all but what is left; the repeat cannot be covered and is refused before any read.
	path := "/v1/ohlc?base=native&quote=fiat:USD&interval=1m&from=" + from
	ts, reader := newSeriesCostServer(t, fiat+oneBook)
	if resp := mustGet(t, ts.URL+path); resp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("first request into a fresh window was refused")
	}
	before := reader.reads.Load()
	if resp := mustGet(t, ts.URL+path); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("unaffordable series: status %d, want 429", resp.StatusCode)
	}
	if n := reader.reads.Load() - before; n != 0 {
		t.Errorf("refused series still issued %d book reads", n)
	}
}
