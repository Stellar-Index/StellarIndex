package v1_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// listCountingAssetsReader counts the listing reads that reach the
// store, so a test can assert a rate-limit denial stopped the read
// rather than merely relabelling its response.
type listCountingAssetsReader struct {
	paginatingAssetsReader
	lists atomic.Int64
}

func (r *listCountingAssetsReader) ListAssetsExt(ctx context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	r.lists.Add(1)
	return r.paginatingAssetsReader.ListAssetsExt(ctx, opts)
}

// newAssetsLimitedServer wires /v1/assets behind the production limiter
// constructor over a Redis-backed anonymous bucket. withStore selects
// whether an AssetsReader is wired.
func newAssetsLimitedServer(t *testing.T, anonLimit int, withStore bool) (*testServerImpl, *listCountingAssetsReader) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	opts := v1.Options{
		RateLimit: middleware.RateLimitBySubject(
			ratelimit.New(rdb, anonLimit, time.Minute), nil, middleware.SkipHealthAndMetrics, nil),
	}
	reader := &listCountingAssetsReader{paginatingAssetsReader: paginatingAssetsReader{total: 3}}
	if withStore {
		opts.AssetsReader = reader
	}
	return startHTTPTest(t, v1.New(opts).Handler()), reader
}

// TestAssetList_ChargesByThePlanSelected is the K009 regression for the
// second surface. /v1/assets is one route and several query plans, the
// query string picks the plan, and the limiter charged all of them one
// token — so the volume-ranked plan (measured 18x the default) and the
// cache-defeating `q` scan were bought at the cheapest plan's price.
//
// Each case is one request into a fresh 100-token window; the assertion
// is the exact post-charge remainder.
func TestAssetList_ChargesByThePlanSelected(t *testing.T) {
	cases := []struct {
		name, query   string
		wantStatus    int
		wantRemaining string
	}{
		{"default listing", "?limit=5", 200, "99"},
		{"explicit default order", "?order_by=observation_count_desc", 200, "99"},
		{"volume-ranked plan", "?order_by=volume_24h_usd_desc", 200, "90"},
		{"search", "?q=usd", 200, "95"},
		{"search on the volume-ranked plan", "?q=usd&order_by=volume_24h_usd_desc", 200, "86"},
		// asset_class=all reaches the SAME volume-ranked store read
		// without naming order_by; pricing only the named parameter would
		// leave this as the way around the charge.
		{"unified listing", "?asset_class=all", 200, "90"},
		{"unified listing + search", "?asset_class=all&q=usd", 200, "86"},
		// The class-scoped listings filter a few dozen curated rows
		// in-process and ignore q: no plan selected, base token.
		{"class-scoped listing ignores q", "?asset_class=stablecoin&q=usd", 200, "99"},
		// Rejected before any plan is chosen: base token only.
		{"invalid order_by", "?order_by=bogus", 400, "99"},
		{"order_by with asset_class", "?asset_class=all&order_by=volume_24h_usd_desc", 400, "99"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newAssetsLimitedServer(t, 100, true)
			resp := mustGet(t, ts.URL+"/v1/assets"+tc.query)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := resp.Header.Get("X-RateLimit-Remaining"); got != tc.wantRemaining {
				t.Fatalf("X-RateLimit-Remaining = %q, want %q", got, tc.wantRemaining)
			}
		})
	}
}

// TestAssetList_DeniedPlanDoesNoRead: the surcharge lands before the
// read. A caller with 2 tokens left cannot buy a 10-token plan, and the
// store is not touched finding that out.
func TestAssetList_DeniedPlanDoesNoRead(t *testing.T) {
	ts, reader := newAssetsLimitedServer(t, 12, true)
	url := ts.URL + "/v1/assets?order_by=volume_24h_usd_desc&limit=2"

	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "2" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 2", got)
	}

	before := reader.lists.Load()
	resp = mustGet(t, url+"&cursor=")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
	if got := reader.lists.Load(); got != before {
		t.Fatalf("a denied request still ran %d listing read(s)", got-before)
	}
}

// TestAssetList_NoStoreNoSurcharge: with no AssetsReader wired the
// volume-ranked plan does not exist to be selected, and the request
// costs the base token.
func TestAssetList_NoStoreNoSurcharge(t *testing.T) {
	ts, _ := newAssetsLimitedServer(t, 100, false)
	resp := mustGet(t, ts.URL+"/v1/assets?order_by=volume_24h_usd_desc&q=usd")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "99" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 99", got)
	}
}

// TestAssetList_QueryLengthIsBounded: `q` was bounded only by the
// server's header limit while being carried verbatim into the listing
// cache key and three LIKE patterns. 100 bytes clears the longest value
// that can match a row (a 69-byte classic asset id); one byte more is a
// 400 on every path, class-scoped listings included, and reads nothing.
func TestAssetList_QueryLengthIsBounded(t *testing.T) {
	ts, reader := newAssetsLimitedServer(t, 100, true)

	resp := mustGet(t, ts.URL+"/v1/assets?q="+strings.Repeat("a", 100))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("100-byte q: status = %d, want 200", resp.StatusCode)
	}

	before := reader.lists.Load()
	for _, path := range []string{"/v1/assets?q=", "/v1/assets?asset_class=all&q=", "/v1/assets?asset_class=fiat&q="} {
		resp = mustGet(t, ts.URL+path+strings.Repeat("a", 101))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s<101 bytes>: status = %d, want 400", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s: Content-Type = %q, want application/problem+json", path, ct)
		}
	}
	if got := reader.lists.Load(); got != before {
		t.Fatalf("an over-long q still ran %d listing read(s)", got-before)
	}

	// Surrounding whitespace is trimmed before the bound applies.
	resp = mustGet(t, ts.URL+"/v1/assets?q=%20"+strings.Repeat("a", 100)+"%20")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("padded 100-byte q: status = %d, want 200", resp.StatusCode)
	}
}
