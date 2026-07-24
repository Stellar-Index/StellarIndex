// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
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
