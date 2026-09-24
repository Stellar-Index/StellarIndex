// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// TestAuth_FailedAuthThrottle_FailsClosedOnSustainedOutage pins the
// SEC-15 / C3-5 fix: once the failed-auth Bucket has been erroring for
// longer than its dwell-time (Take returns
// [ratelimit.ErrThrottleUnavailable]), the credential-stuffing throttle
// must fail CLOSED (still block the request) instead of silently
// disabling brute-force protection for the rest of the outage. On the
// unfixed code takeFailedAuth returns (false, 0) for ANY limiter error
// — including ErrThrottleUnavailable — so a bad key keeps returning a
// plain 401 forever during a sustained Redis outage.
func TestAuth_FailedAuthThrottle_FailsClosedOnSustainedOutage(t *testing.T) {
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}

	rdb, mr := newRLRedis(t)
	fakeNow := time.Unix(1_750_000_000, 0)
	limiter := ratelimit.New(rdb, 3, time.Minute,
		ratelimit.WithClock(func() time.Time { return fakeNow }),
		ratelimit.WithDwellTime(30*time.Second),
	)

	mw := middleware.Auth(middleware.AuthOptions{
		Mode:              middleware.AuthModeAPIKey,
		APIKey:            stubAPIKeyValidator{knownKey: "good-key"},
		FailedAuthLimiter: limiter,
	})
	h := mw(okHandler())

	badReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
		r.RemoteAddr = "203.0.113.55:44440"
		r.Header.Set("X-API-Key", "wrong-key")
		return r
	}

	// Blow up the backing miniredis — every future Take() call errors.
	mr.Close()

	// First failure only ARMS the dwell-time clock (elapsed=0 < 30s):
	// the bucket still returns a plain wrapped error, so the throttle
	// stays fail-open for this one request — matches the main
	// RateLimit middleware's grace window (F-0050/F-0150).
	w := httptest.NewRecorder()
	h.ServeHTTP(w, badReq())
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("first failure inside dwell window: status = %d, want 401", w.Code)
	}

	// Advance the fake clock past the dwell-time: Take() now returns
	// ErrThrottleUnavailable. The failed-auth throttle must fail CLOSED
	// (block with 429 + Retry-After) rather than let credential
	// guessing continue unbounded for the rest of the outage.
	fakeNow = fakeNow.Add(31 * time.Second)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, badReq())
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("sustained outage: status = %d, want 429 (fail-closed, not a bare 401)", w2.Code)
	}
	if ra := w2.Header().Get("Retry-After"); ra == "" {
		t.Error("fail-closed response during sustained outage must carry Retry-After")
	}
}

// TestAuth_FailedAuthThrottle is the C3-5 regression: invalid-credential
// attempts must be throttled PER IP. Auth runs before the main rate
// limiter, so without this a wrong key is rejected (401) before reaching
// any limiter and can be retried without bound (credential stuffing). On
// the unfixed code the (N+1)th bad key from one IP is still a plain 401;
// with the fix it becomes 429.
func TestAuth_FailedAuthThrottle(t *testing.T) {
	// Direct-peer keying: no trusted proxies, so remoteIPFor uses
	// RemoteAddr's host deterministically.
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}

	const budget = 3
	// nil rdb → in-process fallback (no Redis needed for the test).
	limiter := ratelimit.New(nil, budget, time.Minute)

	mw := middleware.Auth(middleware.AuthOptions{
		Mode:              middleware.AuthModeAPIKey,
		APIKey:            stubAPIKeyValidator{knownKey: "good-key"},
		FailedAuthLimiter: limiter,
	})
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := mw(inner)

	badReq := func(remoteAddr string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
		r.RemoteAddr = remoteAddr
		r.Header.Set("X-API-Key", "wrong-key")
		return r
	}

	const ip = "203.0.113.7:44444"

	// First `budget` bad attempts get the ordinary 401.
	for i := 1; i <= budget; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, badReq(ip))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("bad attempt %d: status = %d, want 401", i, w.Code)
		}
	}

	// The (budget+1)th bad attempt from the SAME ip is throttled: 429.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, badReq(ip))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget bad attempt: status = %d, want 429 (per-IP failed-auth throttle)", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Error("429 failed-auth response must carry Retry-After")
	}

	// A DIFFERENT ip still has its full budget — the throttle is per-IP.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, badReq("198.51.100.20:5555"))
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("different IP first attempt: status = %d, want 401 (per-IP isolation)", w2.Code)
	}

	// A VALID key from the throttled ip still passes: successful auth
	// never consumes the failed-auth budget, so the Auth-before-RateLimit
	// ordering for legitimate callers is preserved.
	good := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	good.RemoteAddr = ip
	good.Header.Set("X-API-Key", "good-key")
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, good)
	if w3.Code != http.StatusOK {
		t.Fatalf("valid key from throttled IP: status = %d, want 200 (valid requests are never failed-auth-throttled)", w3.Code)
	}
}

// TestAuth_FailedAuthThrottle_NilLimiterIsNoop confirms that with no
// limiter wired (e.g. failed_auth_rate_limit_per_min=0), bad keys keep
// returning 401 without bound — the throttle is purely additive.
func TestAuth_FailedAuthThrottle_NilLimiterIsNoop(t *testing.T) {
	mw := middleware.Auth(middleware.AuthOptions{
		Mode:   middleware.AuthModeAPIKey,
		APIKey: stubAPIKeyValidator{knownKey: "good-key"},
		// FailedAuthLimiter deliberately nil.
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 10; i++ {
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
		r.RemoteAddr = "203.0.113.9:1234"
		r.Header.Set("X-API-Key", "wrong-key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d with nil limiter: status = %d, want 401 (no throttle)", i, w.Code)
		}
	}
}

// TestAuth_FailedAuthThrottle_PerKeyPrefix is the RSEC-R4 regression: a
// distributed guesser aiming at ONE key from many IPs must still be
// throttled. With an IP-only bucket every guess below lands in a fresh
// per-IP bucket and returns 401 forever; the key-prefix dimension turns
// the (budget+1)th guess at the same prefix into a 429.
func TestAuth_FailedAuthThrottle_PerKeyPrefix(t *testing.T) {
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}
	const budget = 3
	const targetPrefix = "sip_4f9c1d8b"
	validKey := targetPrefix + strings.Repeat("0", 56)
	guess := func(i int) string { return targetPrefix + strings.Repeat("f", 55) + strconv.Itoa(i%10) }

	for _, mode := range []middleware.AuthMode{middleware.AuthModeAPIKey, middleware.AuthModeAPIKeyOptional} {
		t.Run(string(mode), func(t *testing.T) {
			h := middleware.Auth(middleware.AuthOptions{
				Mode:              mode,
				APIKey:            stubAPIKeyValidator{knownKey: validKey},
				FailedAuthLimiter: ratelimit.New(nil, budget, time.Minute),
			})(okHandler())
			req := func(ip int, key string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
				r.RemoteAddr = "198.51.100." + strconv.Itoa(ip) + ":4000"
				r.Header.Set("X-API-Key", key)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}

			for i := 1; i <= budget; i++ {
				if w := req(i, guess(i)); w.Code != http.StatusUnauthorized {
					t.Fatalf("guess %d from a fresh IP: status = %d, want 401", i, w.Code)
				}
			}
			w := req(budget+1, guess(budget+1))
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("guess %d at the same key prefix from a fresh IP: status = %d, want 429 (per-key-prefix throttle)", budget+1, w.Code)
			}
			if w.Header().Get("Retry-After") == "" {
				t.Error("per-key-prefix 429 must carry Retry-After")
			}

			// A guess at a DIFFERENT prefix from a fresh IP is unaffected.
			if w := req(50, "sip_00000000"+strings.Repeat("f", 56)); w.Code != http.StatusUnauthorized {
				t.Fatalf("other prefix: status = %d, want 401 (per-prefix isolation)", w.Code)
			}
			// The key's holder is never locked out by guesses at their
			// prefix: a successful lookup never consults the budget.
			if w := req(60, validKey); w.Code != http.StatusOK {
				t.Fatalf("valid key under an exhausted prefix bucket: status = %d, want 200", w.Code)
			}
		})
	}
}

// TestAuth_FailedAuthTotal_CountsEveryRejection pins the fleet-wide
// tripwire counter behind stellarindex_failed_auth_rate_high: every
// credential rejection increments it by outcome, while a valid key and a
// server-side misconfiguration (503) do not.
func TestAuth_FailedAuthTotal_CountsEveryRejection(t *testing.T) {
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}
	rejected := obs.FailedAuthTotal.WithLabelValues(obs.FailedAuthRejected)
	throttled := obs.FailedAuthTotal.WithLabelValues(obs.FailedAuthThrottled)
	rej0, thr0 := testutil.ToFloat64(rejected), testutil.ToFloat64(throttled)

	h := middleware.Auth(middleware.AuthOptions{
		Mode:              middleware.AuthModeAPIKey,
		APIKey:            stubAPIKeyValidator{knownKey: "good-key"},
		FailedAuthLimiter: ratelimit.New(nil, 1, time.Minute),
	})(okHandler())
	misconfigured := middleware.Auth(middleware.AuthOptions{Mode: middleware.AuthModeAPIKey})(okHandler())
	serve := func(handler http.Handler, key string) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
		r.RemoteAddr = "203.0.113.77:4000"
		r.Header.Set("X-API-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}

	for _, step := range []struct {
		handler http.Handler
		key     string
		want    int
	}{
		{h, "wrong-key", http.StatusUnauthorized},
		{h, "wrong-key", http.StatusTooManyRequests},
		{h, "good-key", http.StatusOK},
		{misconfigured, "wrong-key", http.StatusServiceUnavailable},
	} {
		if got := serve(step.handler, step.key); got != step.want {
			t.Fatalf("key %q: status = %d, want %d", step.key, got, step.want)
		}
	}
	if got := testutil.ToFloat64(rejected) - rej0; got != 1 {
		t.Errorf("rejected delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(throttled) - thr0; got != 1 {
		t.Errorf("throttled delta = %v, want 1", got)
	}
}

// TestAuth_FailedAuthThrottle_SEP10NotPooledByPrefix pins that the
// key-prefix dimension is API-key only. Every JWT starts with the same
// base64 header, so keying SEP-10 failures on their leading bytes would
// pool every bad token server-wide into one bucket.
func TestAuth_FailedAuthThrottle_SEP10NotPooledByPrefix(t *testing.T) {
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}
	const budget = 2
	h := middleware.Auth(middleware.AuthOptions{
		Mode:              middleware.AuthModeSEP10,
		SEP10:             stubSEP10Validator{knownJWT: "good"},
		FailedAuthLimiter: ratelimit.New(nil, budget, time.Minute),
	})(okHandler())
	for i := 1; i <= 3*budget; i++ {
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
		r.RemoteAddr = "198.51.100." + strconv.Itoa(i) + ":4000"
		r.Header.Set("Authorization", "Bearer eyJhbGciOiJFZERTQSJ9.e30.sig"+strconv.Itoa(i))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("bad JWT %d from a fresh IP: status = %d, want 401 (no cross-IP JWT pooling)", i, w.Code)
		}
	}
}
