package metadata_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/RatesEngine/rates-engine/internal/cachekeys"
	"github.com/RatesEngine/rates-engine/internal/metadata"
)

func newCacheRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

// newCacheFixtureServer returns a TLS test server that counts how
// many times it served stellar.toml.
func newCacheFixtureServer(t *testing.T, hits *int64) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/stellar.toml" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(hits, 1)
		_, _ = w.Write([]byte(fixtureTOML))
	}))
}

func TestCache_ReadThroughAndReuse(t *testing.T) {
	var hits int64
	srv := newCacheFixtureServer(t, &hits)
	defer srv.Close()

	rdb, _ := newCacheRedis(t)
	r := newLocalResolver(t, srv)
	c := metadata.NewCache(r, rdb)

	ctx := context.Background()
	dom := hostOf(t, srv)

	// First call — miss, fetches upstream.
	sep, err := c.Resolve(ctx, dom)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if sep.OrgName != "Circle Internet Financial Limited" {
		t.Errorf("OrgName = %q", sep.OrgName)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("upstream hits after first call: %d (want 1)", got)
	}

	// Second call — should land in redis.
	sep2, err := c.Resolve(ctx, dom)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if sep2.OrgName != sep.OrgName {
		t.Errorf("OrgName mismatch across calls: %q vs %q", sep.OrgName, sep2.OrgName)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("upstream hits after second call: %d (want still 1 — cached)", got)
	}
}

func TestCache_InvalidateForcesRefresh(t *testing.T) {
	var hits int64
	srv := newCacheFixtureServer(t, &hits)
	defer srv.Close()

	rdb, _ := newCacheRedis(t)
	c := metadata.NewCache(newLocalResolver(t, srv), rdb)

	ctx := context.Background()
	dom := hostOf(t, srv)

	if _, err := c.Resolve(ctx, dom); err != nil {
		t.Fatal(err)
	}
	if err := c.Invalidate(ctx, dom); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, err := c.Resolve(ctx, dom); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Errorf("upstream hits after invalidate+re-resolve: %d (want 2)", got)
	}
}

func TestCache_NegativeResultsNotCached(t *testing.T) {
	// 404-only server; cache should fall through every call.
	var hits int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	rdb, _ := newCacheRedis(t)
	c := metadata.NewCache(newLocalResolver(t, srv), rdb)

	ctx := context.Background()
	dom := hostOf(t, srv)

	if _, err := c.Resolve(ctx, dom); err == nil {
		t.Fatal("first Resolve: want error")
	}
	if _, err := c.Resolve(ctx, dom); err == nil {
		t.Fatal("second Resolve: want error")
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Errorf("upstream hits (negative-cache check): %d (want 2 — errors must not cache)", got)
	}
}

func TestCache_TTLExpiry(t *testing.T) {
	var hits int64
	srv := newCacheFixtureServer(t, &hits)
	defer srv.Close()

	rdb, mr := newCacheRedis(t)
	c := metadata.NewCache(newLocalResolver(t, srv), rdb)

	ctx := context.Background()
	dom := hostOf(t, srv)

	if _, err := c.Resolve(ctx, dom); err != nil {
		t.Fatal(err)
	}
	// Fast-forward past TTL.
	mr.FastForward(cachekeys.TOMLTTL + time.Second)

	if _, err := c.Resolve(ctx, dom); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Errorf("upstream hits after TTL expiry: %d (want 2)", got)
	}
}

func TestCache_NilRedisFallsThrough(t *testing.T) {
	// NewCache(r, nil) should still work — it just always calls
	// upstream. Useful for local dev without redis.
	var hits int64
	srv := newCacheFixtureServer(t, &hits)
	defer srv.Close()

	c := metadata.NewCache(newLocalResolver(t, srv), nil)

	ctx := context.Background()
	dom := hostOf(t, srv)

	for i := 0; i < 3; i++ {
		if _, err := c.Resolve(ctx, dom); err != nil {
			t.Fatalf("Resolve %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&hits); got != 3 {
		t.Errorf("upstream hits with nil redis: %d (want 3 — no caching)", got)
	}
}
