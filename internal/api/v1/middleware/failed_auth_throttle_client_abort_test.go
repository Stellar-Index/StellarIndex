// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// TestAuth_FailedAuthThrottle_ClientAbortsDoNotArmFailClosed pins the
// Q153 hazard on the failed-auth (credential-stuffing) throttle: unlike
// [middleware.RateLimit] / [middleware.RateLimitBySubject], which detach
// from the request's cancellation via throttleContext before calling
// into [ratelimit.Bucket] (REL-06 F059, reverification-2026-09-18),
// takeFailedAuth passed r.Context() straight into limiter.Take. A
// client that opens a connection, sends a bad API key and RSTs before
// the Redis round-trip completes hands the bucket a context.Canceled
// error, which [ratelimit.Bucket] cannot distinguish from a Redis
// failure — every abort arms the dwell clock. Enough aborts in a row
// trip ErrThrottleUnavailable on a perfectly healthy Redis, and the
// failed-auth throttle fails CLOSED (429) for a well-behaved bad-key
// attempt that should just get a plain 401.
func TestAuth_FailedAuthThrottle_ClientAbortsDoNotArmFailClosed(t *testing.T) {
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}

	rdb, _ := newRLRedis(t)
	clock := newManualClock()
	limiter := ratelimit.New(rdb, 100, time.Minute, ratelimit.WithClock(clock.now))

	mw := middleware.Auth(middleware.AuthOptions{
		Mode:              middleware.AuthModeAPIKey,
		APIKey:            stubAPIKeyValidator{knownKey: "good-key"},
		FailedAuthLimiter: limiter,
	})
	h := mw(okHandler())

	abortedBadReq := func() *http.Request {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil).WithContext(ctx)
		r.RemoteAddr = "203.0.113.55:44440"
		r.Header.Set("X-API-Key", "wrong-key")
		return r
	}

	// First abort: pre-fix, this is where the dwell clock gets armed.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, abortedBadReq())
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("first aborted bad-key attempt: status = %d, want 401 (Redis is healthy)", w.Code)
	}

	// Past the dwell window, with nothing but aborts in between.
	clock.advance(ratelimit.DefaultDwellTime + time.Second)

	// The decisive request: still just a bad key against a healthy
	// Redis. Must be a plain 401, never a 429/503 manufactured purely
	// by the aborts.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, abortedBadReq())
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("bad-key attempt after abort-only dwell window: status = %d, want 401 — "+
			"client aborts armed the fail-closed dwell clock on the failed-auth throttle "+
			"while Redis was healthy", w2.Code)
	}
}
