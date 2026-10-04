package v1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A carried-forward or partial 200 must leave as no-store so a shared cache
// cannot keep serving it for its route's full band after the origin
// recovers; the healthy answer on the same route keeps the band.

func TestWriteEnvelope_DegradedIsNoStoreAndNeverOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		name     string
		degraded bool
		want     string
	}{
		{"healthy keeps the route band", false, "public, max-age=60, s-maxage=300"},
		{"degraded overrides the band", true, "no-store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := middleware.CacheControl(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, []string{}, Flags{Degraded: tc.degraded})
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/pools", nil))
			if got := rec.Header().Get("Cache-Control"); got != tc.want {
				t.Errorf("Cache-Control = %q, want %q", got, tc.want)
			}
			if strings.Contains(rec.Body.String(), "degraded") {
				t.Errorf("internal marker leaked onto the wire: %s", rec.Body.String())
			}
		})
	}
}

// serveCacheControl runs one request against h and returns the
// Cache-Control the handler itself set (no middleware: "" means the
// route's band would stand).
func serveCacheControl(t *testing.T, h http.HandlerFunc, target string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200: %s", target, rec.Code, rec.Body.String())
	}
	return rec.Header().Get("Cache-Control")
}

func TestHandleSDEXOrderbook_StalledBookIsNoStore(t *testing.T) {
	const usdc = "USDC-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	reader := &stubOfferBookReader{
		offers: []clickhouse.LiveOffer{bookOffer("k1", 1, 100, "native", usdc, 1, 2, 10<<32|7)},
		cursor: 10,
	}
	c := NewSDEXOrderBookCache(reader, nil)
	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := New(Options{SDEXOrderBook: c})
	target := "/v1/sdex/orderbook?selling=native&buying=" + usdc

	if got := serveCacheControl(t, s.handleSDEXOrderbook, target); got == "no-store" {
		t.Errorf("fresh book: Cache-Control = %q, want the route band", got)
	}
	c.mu.Lock()
	c.updated = time.Now().Add(-(SDEXOrderBookStaleAfter + time.Minute))
	c.mu.Unlock()
	if got := serveCacheControl(t, s.handleSDEXOrderbook, target); got != "no-store" {
		t.Errorf("stalled book: Cache-Control = %q, want no-store", got)
	}
}

func TestHandleLiquidityPools_CarriedForwardListingIsNoStore(t *testing.T) {
	reader := &lpCacheReader{}
	s := newLPCacheServer(reader)
	const target = "/v1/liquidity-pools"
	rescanIdle := func() bool { return !s.nativeLPRefreshing.Load() }

	if got := serveCacheControl(t, s.handleLiquidityPools, target); got == "no-store" {
		t.Errorf("cold fill: Cache-Control = %q, want the route band", got)
	}
	if got := serveCacheControl(t, s.handleLiquidityPools, target); got == "no-store" {
		t.Errorf("fresh hit: Cache-Control = %q, want the route band", got)
	}

	reader.fail.Store(true)
	backdateLPEntry(s, 2*nativeLPListingTTL)
	if got := serveCacheControl(t, s.handleLiquidityPools, target); got != "no-store" {
		t.Errorf("lapsed entry: Cache-Control = %q, want no-store", got)
	}
	waitForLPCache(t, "detached rescan", rescanIdle)
	if got := serveCacheControl(t, s.handleLiquidityPools, target); got != "no-store" {
		t.Errorf("last-good after a failed rescan: Cache-Control = %q, want no-store", got)
	}
	waitForLPCache(t, "detached rescan", rescanIdle)
}

func TestFillNativeLPListing_LastGoodAfterFailureIsDegraded(t *testing.T) {
	reader := &lpCacheReader{}
	s := newLPCacheServer(reader)
	if _, _, degraded, err := s.fillNativeLPListing(context.Background()); err != nil || degraded {
		t.Fatalf("first fill: degraded=%v err=%v, want a fresh listing", degraded, err)
	}
	reader.fail.Store(true)
	backdateLPEntry(s, 2*nativeLPListingTTL)
	rows, _, degraded, err := s.fillNativeLPListing(context.Background())
	if err != nil || len(rows) != 1 || !degraded {
		t.Fatalf("failed rescan: rows=%d degraded=%v err=%v, want the last-good row marked degraded", len(rows), degraded, err)
	}
}

// swrMarketsUpstream serves one pool and one market; the sparkline batch
// always fails.
type swrMarketsUpstream struct {
	MarketsReader // nil: only the methods below are reached
}

func (swrMarketsUpstream) AllPools(context.Context, timescale.PoolsFilter, string, int, timescale.MarketsOrder) ([]Pool, string, error) {
	return []Pool{{Source: "soroswap", Base: "native", Quote: "crypto:BTC"}}, "", nil
}

func (swrMarketsUpstream) DistinctPairsExt(context.Context, string, int, timescale.MarketsOrder) ([]Market, string, error) {
	return []Market{{Base: "native", Quote: "crypto:BTC"}}, "", nil
}

func (swrMarketsUpstream) GetPairsVolumeHistory24hBatch(context.Context, [][2]string) (map[string][]timescale.PairVolumePoint, error) {
	return nil, errors.New("sparkline read failed")
}

// expireMarketsCache ages every entry past the TTL and waits out any
// detached refresh a previous serve kicked.
func expireMarketsCache(t *testing.T, c *CachedMarketsReader) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c.mu.Lock()
		busy := false
		for _, e := range c.entries {
			busy = busy || e.flight != nil
		}
		if !busy {
			for _, e := range c.entries {
				e.at = time.Now().Add(-2 * c.ttl)
			}
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("markets cache refresh never settled")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestHandlePools_StaleWhileRevalidateServeIsNoStore(t *testing.T) {
	c := NewCachedMarketsReader(swrMarketsUpstream{}, time.Minute)
	s := New(Options{Markets: c})
	const target = "/v1/pools"

	if got := serveCacheControl(t, s.handlePools, target); got == "no-store" {
		t.Errorf("cold fill: Cache-Control = %q, want the route band", got)
	}
	expireMarketsCache(t, c)
	if got := serveCacheControl(t, s.handlePools, target); got != "no-store" {
		t.Errorf("stale-while-revalidate serve: Cache-Control = %q, want no-store", got)
	}
	expireMarketsCache(t, c)
}

func TestHandleMarkets_StaleOrPartialPageIsNoStore(t *testing.T) {
	c := NewCachedMarketsReader(swrMarketsUpstream{}, time.Minute)
	s := New(Options{Markets: c})

	if got := serveCacheControl(t, s.handleMarkets, "/v1/markets"); got == "no-store" {
		t.Errorf("cold fill: Cache-Control = %q, want the route band", got)
	}
	if got := serveCacheControl(t, s.handleMarkets, "/v1/markets?include=sparkline"); got != "no-store" {
		t.Errorf("failed sparkline enrichment: Cache-Control = %q, want no-store", got)
	}
	expireMarketsCache(t, c)
	if got := serveCacheControl(t, s.handleMarkets, "/v1/markets"); got != "no-store" {
		t.Errorf("stale-while-revalidate serve: Cache-Control = %q, want no-store", got)
	}
	expireMarketsCache(t, c)
}

type flakySourcesStats struct{ fail bool }

func (f flakySourcesStats) GetSourceStats(context.Context) ([]timescale.SourceStats, error) {
	if f.fail {
		return nil, errors.New("stats read failed")
	}
	return nil, nil
}

func (flakySourcesStats) GetSourceVolumeHistory24h(context.Context) ([]timescale.SourceVolumeBucket, error) {
	return nil, nil
}

func (flakySourcesStats) GetSourceVolumeHistory7d(context.Context) ([]timescale.SourceVolumeBucket, error) {
	return nil, nil
}

func TestHandleSources_DroppedStatsAreNoStore(t *testing.T) {
	const target = "/v1/sources?include=stats"
	healthy := New(Options{SourcesStats: flakySourcesStats{}})
	if got := serveCacheControl(t, healthy.handleSources, target); got == "no-store" {
		t.Errorf("stats read: Cache-Control = %q, want the route band", got)
	}
	failing := New(Options{SourcesStats: flakySourcesStats{fail: true}})
	if got := serveCacheControl(t, failing.handleSources, target); got != "no-store" {
		t.Errorf("stats soft-failed: Cache-Control = %q, want no-store", got)
	}
}

func TestHandleProtocolDetail_StaleServeIsNoStore(t *testing.T) {
	srv := New(Options{ProtocolActivity: prewarmActivityStub{}, ProtocolBespoke: &prewarmBespokeStub{}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	cacheControl := func() string {
		resp, err := http.Get(ts.URL + "/v1/protocols/cctp") //nolint:noctx // test helper
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		return resp.Header.Get("Cache-Control")
	}

	if got := cacheControl(); got != "public, max-age=60" {
		t.Errorf("fresh build: Cache-Control = %q, want the handler's public band", got)
	}
	key := protocolDetailCacheKey("cctp", protocolActivityWindowDays)
	srv.protoDetailMu.Lock()
	e, ok := srv.protoDetailCache[key]
	if !ok {
		srv.protoDetailMu.Unlock()
		t.Fatal("entry missing after cold build")
	}
	e.at = e.at.Add(-protocolDetailTTL - time.Minute)
	srv.protoDetailCache[key] = e
	srv.protoDetailMu.Unlock()

	if got := cacheControl(); got != "no-store" {
		t.Errorf("stale serve: Cache-Control = %q, want no-store", got)
	}
	waitProtoDetailIdle(t, srv, key)
}
