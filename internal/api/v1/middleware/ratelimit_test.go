package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
	"github.com/Stellar-Index/StellarIndex/internal/redistest"
)

func newRLRedis(t *testing.T) (*redis.Client, *redistest.Server) {
	t.Helper()
	s := redistest.Run(t)
	return s.Client, s
}

// newDownRedis returns a client whose every dial fails, in one attempt.
// The address of a closed miniredis is not a down Redis: during go-redis's
// dial retries another process on the runner can bind the freed port and
// answer the script, so the "failed" take succeeds.
func newDownRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{
		Addr:          "redis-down.invalid:6379",
		MaxRetries:    -1,
		DialerRetries: 1,
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("redis down")
		},
	})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// okHandler always returns 200 OK so the middleware's effect is
// easy to observe.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// fixedKeyFn returns a constant key — easier than threading a real
// remote-IP through the test server.
func fixedKeyFn(k string) func(*http.Request) string {
	return func(*http.Request) string { return k }
}

func TestRateLimit_AllowsUnderLimit(t *testing.T) {
	rdb, _ := newRLRedis(t)
	b := ratelimit.New(rdb, 3, time.Minute)

	h := middleware.RateLimit(b, fixedKeyFn("k1"), nil, nil)(okHandler())

	for i := 0; i < 3; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("attempt %d: status = %d", i+1, w.Code)
		}
		if got := w.Header().Get("X-RateLimit-Limit"); got != "3" {
			t.Errorf("X-RateLimit-Limit = %q, want 3", got)
		}
		wantRemaining := strconv.Itoa(3 - (i + 1))
		if got := w.Header().Get("X-RateLimit-Remaining"); got != wantRemaining {
			t.Errorf("attempt %d: X-RateLimit-Remaining = %q, want %q", i+1, got, wantRemaining)
		}
	}
}

func TestRateLimit_Rejects429AfterLimit(t *testing.T) {
	rdb, _ := newRLRedis(t)
	b := ratelimit.New(rdb, 2, time.Minute)

	h := middleware.RateLimit(b, fixedKeyFn("k2"), nil, nil)(okHandler())

	// Exhaust the budget.
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
	}

	// Over-limit request.
	r := httptest.NewRequest(http.MethodGet, "/some-path", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Error("Retry-After header missing")
	}
	var p struct {
		Type, Title, Instance string
		Status                int
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	if p.Status != 429 {
		t.Errorf("problem.status = %d, want 429", p.Status)
	}
	if p.Instance != "/some-path" {
		t.Errorf("problem.instance = %q, want /some-path", p.Instance)
	}
	// 429s must override any per-route Cache-Control directive
	// (e.g. /v1/assets is `public, max-age=60, s-maxage=300`); a CDN
	// would otherwise replay the denial to other clients on the same
	// cache key.
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a 429", cc)
	}
}

// X-RateLimit-Reset is required for clients to back off proactively
// (GitHub/Twitter convention: Unix-epoch seconds when the bucket
// resets). Pin the header presence + future-tense semantics so a
// refactor can't silently regress the value to "now" or omit it.
func TestRateLimit_EmitsXRateLimitResetHeader(t *testing.T) {
	rdb, _ := newRLRedis(t)
	b := ratelimit.New(rdb, 5, time.Minute)

	h := middleware.RateLimit(b, fixedKeyFn("k-reset"), nil, nil)(okHandler())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	now := time.Now().Unix()
	h.ServeHTTP(w, r)

	got := w.Header().Get("X-RateLimit-Reset")
	if got == "" {
		t.Fatal("X-RateLimit-Reset header missing")
	}
	resetAt, err := strconv.ParseInt(got, 10, 64)
	if err != nil {
		t.Fatalf("X-RateLimit-Reset = %q: not an integer", got)
	}
	// For a 60s window, reset must be in (now, now + 60] seconds.
	// Equality on the upper bound covers the edge case where the
	// request lands exactly at a window boundary.
	if resetAt <= now {
		t.Errorf("X-RateLimit-Reset = %d, want > %d (must be future)", resetAt, now)
	}
	if resetAt > now+60 {
		t.Errorf("X-RateLimit-Reset = %d, want ≤ %d (≤ window length)", resetAt, now+60)
	}
}

func TestRateLimit_EmptyKeyBypasses(t *testing.T) {
	rdb, _ := newRLRedis(t)
	b := ratelimit.New(rdb, 1, time.Minute)

	called := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.RateLimit(b, fixedKeyFn(""), nil, nil)(inner)

	for i := 0; i < 5; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("empty-key request %d rejected: %d", i+1, w.Code)
		}
	}
	if called != 5 {
		t.Errorf("inner called %d times, want 5", called)
	}
}

func TestRateLimit_SkipsWhenSkipReturnsTrue(t *testing.T) {
	rdb, _ := newRLRedis(t)
	b := ratelimit.New(rdb, 1, time.Minute)

	h := middleware.RateLimit(
		b, fixedKeyFn("k3"),
		func(r *http.Request) bool { return r.URL.Path == "/v1/healthz" },
		nil,
	)(okHandler())

	// Budget is 1. Call /v1/healthz 10× — none should count.
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest(http.MethodGet, "/v1/healthz", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("healthz request %d rejected: %d", i+1, w.Code)
		}
	}
	// Now a regular request should still get its one allowance.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("first non-skipped request rejected: %d", w.Code)
	}
}

// TestRateLimit_TruncatesLongKeys guards the MaxRateLimitKeyLen cap on
// RateLimit's entry point: two keys that agree on the first
// MaxRateLimitKeyLen bytes and differ only after it must collapse onto
// the same bucket, or a hostile caller mints unlimited distinct buckets
// by varying an oversize key past the cap (e.g. via a crafted
// X-Forwarded-For with a custom KeyFn).
func TestRateLimit_TruncatesLongKeys(t *testing.T) {
	rdb, _ := newRLRedis(t)
	b := ratelimit.New(rdb, 1, time.Minute)
	longShared := strings.Repeat("a", middleware.MaxRateLimitKeyLen+50)

	h := middleware.RateLimit(b, fixedKeyFn(longShared+"AAAA"), nil, nil)(okHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", w.Code)
	}

	h2 := middleware.RateLimit(b, fixedKeyFn(longShared+"BBBB"), nil, nil)(okHandler())
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("second request (key differs only past MaxRateLimitKeyLen) status = %d, want 429 — key not truncated", w.Code)
	}
}

func TestRateLimit_FailsOpenOnRedisError(t *testing.T) {
	rdb := newDownRedis(t)
	b := ratelimit.New(rdb, 1, time.Minute)

	h := middleware.RateLimit(b, fixedKeyFn("k4"), nil, nil)(okHandler())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("fail-open violated: status = %d", w.Code)
	}
	// Rate-limit headers should be absent (we didn't compute them).
	if got := w.Header().Get("X-RateLimit-Limit"); got != "" {
		t.Errorf("X-RateLimit-Limit should be absent on failure, got %q", got)
	}
}

// A sustained Redis outage flips the limiter to fail-closed 503s; each one
// must be counted, since the fail-open counter stops moving at that point.
func TestRateLimit_FailClosedPastDwellIsCounted(t *testing.T) {
	rdb := newDownRedis(t)
	clock := newManualClock()
	b := ratelimit.New(rdb, 1, time.Minute, ratelimit.WithClock(clock.now))
	h := middleware.RateLimit(b, fixedKeyFn("k-closed"), nil, nil)(okHandler())
	closed := obs.RateLimitFailClosedTotal.WithLabelValues(obs.RateLimiterAPI)
	before := testutil.ToFloat64(closed)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("inside dwell: status = %d, want 200 (fail-open)", w.Code)
	}
	if got := testutil.ToFloat64(closed) - before; got != 0 {
		t.Fatalf("fail-open request counted as fail-closed: delta = %v", got)
	}

	clock.advance(ratelimit.DefaultDwellTime + time.Second)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("past dwell: status = %d, want 503", w.Code)
	}
	if got := testutil.ToFloat64(closed) - before; got != 1 {
		t.Fatalf("fail_closed_total{limiter=api} delta = %v, want 1", got)
	}
}

func TestRateLimitBySubject_AuthenticatedUsesAuthBucket(t *testing.T) {
	rdb, _ := newRLRedis(t)
	anonBucket := ratelimit.New(rdb, 1, time.Minute)
	authBucket := ratelimit.New(rdb, 2, time.Minute)

	h := middleware.RateLimitBySubject(anonBucket, authBucket, nil, nil)(okHandler())
	reqWithSubject := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		subject := auth.Subject{
			Identifier: "GABC123",
			Tier:       auth.TierSEP10,
		}
		return r.WithContext(auth.WithSubject(r.Context(), subject))
	}

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, reqWithSubject())
		if w.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200", i+1, w.Code)
		}
		if got := w.Header().Get("X-RateLimit-Limit"); got != "2" {
			t.Fatalf("attempt %d: X-RateLimit-Limit = %q, want 2", i+1, got)
		}
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqWithSubject())
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit status = %d, want 429", w.Code)
	}
}

func TestRateLimitBySubject_PerKeyOverrideRaisesLimit(t *testing.T) {
	// Default authBucket caps at 2/min; the subject's APIKey record
	// carries RateLimitPerMin=5 (paid-tier custom plan). The
	// middleware must honour the override — both in the wire-side
	// X-RateLimit-Limit header and the actual allow/deny decision.
	rdb, _ := newRLRedis(t)
	anonBucket := ratelimit.New(rdb, 1, time.Minute)
	authBucket := ratelimit.New(rdb, 2, time.Minute)

	h := middleware.RateLimitBySubject(anonBucket, authBucket, nil, nil)(okHandler())
	reqWithSubject := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		subject := auth.Subject{
			Identifier:      "owner-paid",
			Tier:            auth.TierAPIKey,
			KeyID:           "kid_paid",
			RateLimitPerMin: 5,
		}
		return r.WithContext(auth.WithSubject(r.Context(), subject))
	}

	for i := 1; i <= 5; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, reqWithSubject())
		if w.Code != http.StatusOK {
			t.Fatalf("hit %d: status = %d, want 200 (override should permit 5/min)", i, w.Code)
		}
		if got := w.Header().Get("X-RateLimit-Limit"); got != "5" {
			t.Errorf("hit %d: X-RateLimit-Limit = %q, want 5 (override surfaced on wire)", i, got)
		}
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqWithSubject())
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("6th hit status = %d, want 429 (override should still cap at 5)", w.Code)
	}
}

func TestRateLimitBySubject_AnonymousUsesAnonBucket(t *testing.T) {
	rdb, _ := newRLRedis(t)
	anonBucket := ratelimit.New(rdb, 1, time.Minute)
	authBucket := ratelimit.New(rdb, 3, time.Minute)

	h := middleware.RateLimitBySubject(anonBucket, authBucket, nil, nil)(okHandler())
	reqWithSubject := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		subject := auth.Anonymous("anon-hash-1")
		return r.WithContext(auth.WithSubject(r.Context(), subject))
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqWithSubject())
	if w.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got != "1" {
		t.Fatalf("X-RateLimit-Limit = %q, want 1", got)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, reqWithSubject())
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", w.Code)
	}
}

// TestRateLimitBySubject_AnonymousSameIPDifferentUADoesNotBypass is
// the regression: the anonymous bucket must be keyed on the
// resolved client IP ALONE. A key that folds in a
// sha256(IP|User-Agent) hash lets a client rotate its
// User-Agent on every request to mint unlimited distinct buckets and
// sail past the per-IP anonymous throttle. Here two requests from the
// SAME IP carry DIFFERENT User-Agents; with a budget of 1 the second
// MUST be 429'd because both share one bucket.
func TestRateLimitBySubject_AnonymousSameIPDifferentUADoesNotBypass(t *testing.T) {
	rdb, _ := newRLRedis(t)
	anonBucket := ratelimit.New(rdb, 1, time.Minute)
	authBucket := ratelimit.New(rdb, 3, time.Minute)

	h := middleware.RateLimitBySubject(anonBucket, authBucket, nil, nil)(okHandler())

	// Same source IP; the two requests differ ONLY in User-Agent. The
	// anonymous Subject.Identifier folds in the UA (sha256(IP|UA)), so
	// if the bucket keyed on Identifier these would be two buckets.
	reqWithUA := func(ua string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "203.0.113.7:5555"
		r.Header.Set("User-Agent", ua)
		// Mirror how the Auth middleware attaches the anonymous
		// Subject: Identifier = per-IP+UA hash, Tier = anonymous.
		subject := auth.Anonymous("anon-hash-for-" + ua)
		return r.WithContext(auth.WithSubject(r.Context(), subject))
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqWithUA("curl/8.0"))
	if w.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, reqWithUA("Mozilla/5.0 (rotated)"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request (rotated UA, same IP) status = %d, want 429 — "+
			"UA rotation bypassed the per-IP anonymous throttle (F-1335)", w.Code)
	}
}

// TestRateLimit_NilKeyFnMasksIPv6To64 pins the default (nil) keyFn of
// the single-bucket RateLimit middleware to the forge-resistant,
// /64-masked throttle resolver — the SAME resolver the production anon
// path uses. A default of raw RemoteIPFrom(r) (the
// unmasked /128 read from context) would let a caller could rotate its IPv6
// address within a single delegated /64 to mint a fresh bucket per
// request and bypass the per-IP limit entirely.
//
// Two requests carry DIFFERENT /128 addresses that share the same /64;
// with a budget of 1 the second MUST be 429'd because both mask to the
// same key. A genuinely different /64 keeps its own budget.
//
// Against a RemoteIPFrom(r)-based default this FAILS: it reads the
// remote_ip context value (never populated here — no Logger
// middleware), yielding an empty key, so every request bypasses the
// limiter and the second returns 200 instead of 429.
func TestRateLimit_NilKeyFnMasksIPv6To64(t *testing.T) {
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}

	rdb, _ := newRLRedis(t)
	b := ratelimit.New(rdb, 1, time.Minute)

	// nil keyFn → the default resolver under test.
	h := middleware.RateLimit(b, nil, nil, nil)(okHandler())

	reqFromIPv6 := func(addr string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "[" + addr + "]:5555"
		return r
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqFromIPv6("2001:db8:1234:5678::1"))
	if w.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", w.Code)
	}

	// Different /128, SAME /64 (2001:db8:1234:5678::/64) — must share
	// the bucket and be denied.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, reqFromIPv6("2001:db8:1234:5678:ffff:ffff:ffff:ffff"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request (different /128, same /64) status = %d, want 429 — "+
			"default keyFn failed to mask IPv6 to /64 (SEC-15)", w.Code)
	}

	// A genuinely different /64 must still get its own fresh budget.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, reqFromIPv6("2001:db8:1234:9999::1"))
	if w.Code != http.StatusOK {
		t.Fatalf("request from a different /64 status = %d, want 200 (independent bucket)", w.Code)
	}
}

func TestSkipHealthAndMetrics(t *testing.T) {
	cases := map[string]bool{
		"/v1/healthz":          true,
		"/v1/readyz":           true,
		"/v1/livez/lake":       true, // lake-route LB probe must not spend the anon bucket (api-security-3)
		"/v1/version":          true,
		"/metrics":             true,
		"/robots.txt":          true,
		"/":                    true,
		"/errors/rate-limited": true, // must stay in lockstep with isUnauthenticatedInfraPath
		"/v1/assets":           false,
		"/v1/price":            false,
		"/v1/metrics-fake":     false,
	}
	for path, want := range cases {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if got := middleware.SkipHealthAndMetrics(r); got != want {
			t.Errorf("SkipHealthAndMetrics(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestRateLimitBySubject_SkipsWhenSkipReturnsTrue proves the skip block
// (ratelimit.go's `if skip != nil && skip(r)`) is exercised on
// RateLimitBySubject — the ONLY entry point production wires
// (cmd/stellarindex-api/main.go), with the actual SkipHealthAndMetrics
// predicate production uses. TestRateLimit_SkipsWhenSkipReturnsTrue
// drives the same block on RateLimit, which production never
// constructs (server.go declares it test-only), so it left this path
// at 0% coverage: deleting the skip check inside RateLimitBySubject
// left internal/api/v1, internal/ratelimit and cmd/stellarindex-api all
// green.
func TestRateLimitBySubject_SkipsWhenSkipReturnsTrue(t *testing.T) {
	rdb, _ := newRLRedis(t)
	anonBucket := ratelimit.New(rdb, 1, time.Minute)
	authBucket := ratelimit.New(rdb, 1, time.Minute)

	h := middleware.RateLimitBySubject(anonBucket, authBucket, middleware.SkipHealthAndMetrics, nil)(okHandler())

	// Budget is 1. Call /v1/healthz 10x — none should count.
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest(http.MethodGet, "/v1/healthz", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("healthz request %d rejected: %d", i+1, w.Code)
		}
	}

	// A non-skipped path (NOT "/" — SkipHealthAndMetrics treats the bare
	// root as an infra probe too) still gets its one allowance...
	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("first non-skipped request rejected: %d", w.Code)
	}
	// ...and is throttled past it, proving the skip bypassed only the
	// health path rather than disabling the bucket entirely.
	r = httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("second non-skipped request status = %d, want 429", w.Code)
	}
}

// TestRateLimitBySubject_TruncatesLongAuthKeys guards the
// MaxRateLimitKeyLen cap at RateLimitBySubject's own truncation site
// (distinct from RateLimit's — TestRateLimit_TruncatesLongKeys does not
// exercise this one). An authenticated subject's Identifier feeds
// authenticatedRateLimitKey (UsageKeyForSubject) directly; two
// identifiers that agree on the first MaxRateLimitKeyLen bytes of the
// derived key and differ only after it must collapse onto the same
// bucket.
func TestRateLimitBySubject_TruncatesLongAuthKeys(t *testing.T) {
	rdb, _ := newRLRedis(t)
	anonBucket := ratelimit.New(rdb, 1, time.Minute)
	authBucket := ratelimit.New(rdb, 1, time.Minute)

	h := middleware.RateLimitBySubject(anonBucket, authBucket, nil, nil)(okHandler())
	longShared := strings.Repeat("a", middleware.MaxRateLimitKeyLen+50)
	reqWithIdentifier := func(suffix string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		subject := auth.Subject{Identifier: longShared + suffix, Tier: auth.TierSEP10}
		return r.WithContext(auth.WithSubject(r.Context(), subject))
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqWithIdentifier("AAAA"))
	if w.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, reqWithIdentifier("BBBB"))
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("second request (identifier differs only past MaxRateLimitKeyLen) status = %d, want 429 — key not truncated", w.Code)
	}
}

// TestRateLimitBySubject_AnonymousIPv6Slash64SharesBucket is the
// regression: an anonymous caller's throttle key must aggregate IPv6
// addresses to their /64 network prefix, not the full /128. Without
// the fix, an attacker who controls an entire delegated /64 (typical
// residential/mobile ISP allocation) mints a fresh throttle bucket for
// every address in that block and the per-IP anonymous cap never
// engages. Here two requests carry DIFFERENT IPv6 addresses that share
// the same /64; with a budget of 1 the second MUST be 429'd because
// both addresses key to the same bucket.
func TestRateLimitBySubject_AnonymousIPv6Slash64SharesBucket(t *testing.T) {
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}

	rdb, _ := newRLRedis(t)
	anonBucket := ratelimit.New(rdb, 1, time.Minute)
	authBucket := ratelimit.New(rdb, 3, time.Minute)

	h := middleware.RateLimitBySubject(anonBucket, authBucket, nil, nil)(okHandler())

	reqFromIPv6 := func(addr string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "[" + addr + "]:5555"
		subject := auth.Anonymous("anon-hash-for-" + addr)
		return r.WithContext(auth.WithSubject(r.Context(), subject))
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqFromIPv6("2001:db8:1234:5678::1"))
	if w.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", w.Code)
	}

	// Different address, SAME /64 (2001:db8:1234:5678::/64).
	w = httptest.NewRecorder()
	h.ServeHTTP(w, reqFromIPv6("2001:db8:1234:5678:ffff:ffff:ffff:ffff"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request (different address, same /64) status = %d, want 429 — "+
			"per-/64 IPv6 aggregation bypassed (SEC-15)", w.Code)
	}

	// A genuinely different /64 must still have its own fresh budget.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, reqFromIPv6("2001:db8:1234:9999::1"))
	if w.Code != http.StatusOK {
		t.Fatalf("request from a different /64 status = %d, want 200 (independent bucket)", w.Code)
	}
}

// Same property on the single-bucket [middleware.RateLimit] entry point,
// which shares the take-with-request-context shape.
func TestRateLimit_ClientAbortsDoNotArmFailClosed(t *testing.T) {
	rdb, _ := newRLRedis(t)
	clock := newManualClock()
	b := ratelimit.New(rdb, 100, time.Minute, ratelimit.WithClock(clock.now))

	h := middleware.RateLimit(b, fixedKeyFn("abort-k"), nil, nil)(okHandler())

	w := httptest.NewRecorder()
	h.ServeHTTP(w, abortedRequest(t))
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "99" {
		t.Errorf("X-RateLimit-Remaining after an aborted request = %q, want %q", got, "99")
	}

	clock.advance(ratelimit.DefaultDwellTime + time.Second)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, abortedRequest(t))
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("status = 503 — client aborts armed the fail-closed dwell clock on a healthy Redis")
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price?asset=native", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("an innocent caller got %d, want 200", w.Code)
	}
}

// Keying the per-minute limit on KeyID gives every key an
// account held its own full bucket — a 25-key account runs at 25x its
// ceiling, and an operator's 100k/min comp becomes 2.5M/min. The monthly
// quota already counted per account; the rate limit must share that
// identity, so minting or rotating keys cannot multiply the ceiling.
func TestRateLimitBySubject_KeysOnOneAccountShareOneBucket(t *testing.T) {
	rdb, _ := newRLRedis(t)
	authBucket := ratelimit.New(rdb, 100, time.Minute)
	h := middleware.RateLimitBySubject(nil, authBucket, nil, nil)(okHandler())

	const perMin = 3
	serve := func(keyID string) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
		sub := auth.Subject{
			Identifier:      auth.AccountIdentifier("comped-co"),
			Tier:            auth.TierAPIKey,
			KeyID:           keyID,
			RateLimitPerMin: perMin,
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), sub)))
		return w.Code
	}

	keys := []string{"kid_a", "kid_b", "kid_c", "kid_d", "kid_e"}
	allowed := 0
	for round := 0; round < 2; round++ {
		for _, k := range keys {
			if serve(k) == http.StatusOK {
				allowed++
			}
		}
	}
	if allowed != perMin {
		t.Fatalf("%d keys on one account were allowed %d requests in one window, want %d — the "+
			"per-minute ceiling must be the account's, not multiplied by its key count", len(keys), allowed, perMin)
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	other := auth.Subject{Identifier: auth.AccountIdentifier("other-co"), Tier: auth.TierAPIKey, KeyID: "kid_z", RateLimitPerMin: perMin}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), other)))
	if w.Code != http.StatusOK {
		t.Fatalf("a different account got %d, want 200 — accounts must not share a bucket", w.Code)
	}
}

func TestRateLimitBySubject_ClientAbortsDoNotArmFailClosed(t *testing.T) {
	rdb, _ := newRLRedis(t)
	clock := newManualClock()
	b := ratelimit.New(rdb, 100, time.Minute, ratelimit.WithClock(clock.now))

	h := middleware.RateLimitBySubject(b, nil, nil, nil)(okHandler())

	// First abort: arms the dwell clock if the request context leaks into the limiter.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, abortedRequest(t))
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "99" {
		t.Errorf("X-RateLimit-Remaining after an aborted request = %q, want %q — the take must still "+
			"reach a HEALTHY Redis and charge the token; an empty header means it errored out and the "+
			"middleware fell open, which is also unmetered traffic for anyone who aborts", got, "99")
	}

	// Past the 30 s dwell window, with nothing but aborts in between.
	clock.advance(ratelimit.DefaultDwellTime + time.Second)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, abortedRequest(t))
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("status = 503 after two aborted requests %v apart — client aborts armed the "+
			"fail-closed dwell clock, so any client can take the whole bucket's tier offline while "+
			"Redis is healthy", ratelimit.DefaultDwellTime+time.Second)
	}

	// The decisive one: a WELL-BEHAVED caller sharing the bucket.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price?asset=native", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("an innocent caller got %d, want 200 — the abort flood must not fail the throttle "+
			"CLOSED for everyone else on this bucket", w.Code)
	}
}

// Blast-radius guard: detaching from the client's cancellation must not
// detach from the BACKEND's failure. A genuinely broken Redis still has
// to arm the dwell clock and fail closed past the window — that
// inversion is the reason the clock exists.
func TestRateLimitBySubject_RealRedisOutageStillFailsClosed(t *testing.T) {
	rdb, mr := newRLRedis(t)
	clock := newManualClock()
	b := ratelimit.New(rdb, 100, time.Minute, ratelimit.WithClock(clock.now))

	h := middleware.RateLimitBySubject(b, nil, nil, nil)(okHandler())

	mr.Kill() // every take from here on is a real transport failure

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price?asset=native", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("first outage request = %d, want 200 (fail OPEN inside the dwell window)", w.Code)
	}

	clock.advance(ratelimit.DefaultDwellTime + time.Second)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price?asset=native", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("sustained-outage request = %d, want 503 — a real Redis outage past the dwell "+
			"window must still fail CLOSED", w.Code)
	}
}
