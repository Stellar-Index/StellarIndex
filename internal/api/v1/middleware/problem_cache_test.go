// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/StellarIndex/stellar-index/internal/api/v1/middleware"
	"github.com/StellarIndex/stellar-index/internal/auth"
)

// TestProblemResponses_NeverShareCacheable pins the cachecontrol.go
// invariant: every middleware rejection (401/403/429 problem+json)
// must override the route directive with `Cache-Control: no-store`.
//
// The CacheControl middleware sets the route's directive BEFORE the
// auth/policy/quota middlewares run, so on a publicly-cacheable
// route (e.g. /v1/price → `public, max-age=30, s-maxage=60`) a
// rejection writer that forgets the override ships a per-key/per-IP
// denial with shared-cacheable headers — a CDN keyed on the URL
// would replay one caller's 401/403 to everyone else. Regression
// test for the 2026-07-02 finding (writeAuthProblem,
// writeKeyPolicyDenied, writeEmailUnverified, and the monthly-quota
// writer all inherited the public directive).
//
// Each case runs the REAL stack shape: CacheControl outermost, the
// rejecting middleware inside it, exactly as server.go composes them.
func TestProblemResponses_NeverShareCacheable(t *testing.T) {
	failInner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("inner handler must not run on a rejection path")
	})

	cases := []struct {
		name       string
		handler    http.Handler
		req        func() *http.Request
		wantStatus int
	}{
		{
			name: "auth 401 invalid key",
			handler: middleware.Auth(middleware.AuthOptions{
				Mode:   middleware.AuthModeAPIKey,
				APIKey: stubAPIKeyValidator{knownKey: "k1"},
			})(failInner),
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/v1/price?asset=native&quote=fiat:USD", nil)
				r.Header.Set("X-API-Key", "wrong-key")
				return r
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:    "keypolicy 403 ip not allowed",
			handler: middleware.KeyPolicy()(failInner),
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/v1/price?asset=native&quote=fiat:USD", nil)
				r.RemoteAddr = "198.51.100.5:1234"
				sub := auth.Subject{
					Identifier:  "acct:test",
					Tier:        auth.TierAPIKey,
					KeyID:       "kid_test",
					IPAllowlist: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")},
				}
				return r.WithContext(auth.WithSubject(r.Context(), sub))
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:    "email-verified 403 unverified signup key",
			handler: middleware.RequireEmailVerified()(failInner),
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/v1/price?asset=native&quote=fiat:USD", nil)
				sub := auth.Subject{
					Tier:       auth.TierAPIKey,
					KeyID:      "kid_unverified",
					Identifier: "signup-abcdef0123456789",
					// EmailVerifiedAt deliberately zero.
				}
				return r.WithContext(auth.WithSubject(r.Context(), sub))
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "monthly-quota 429 at cap",
			handler: middleware.MonthlyQuota(
				&fakeMTDReader{counts: map[string]int64{"key:K1": 100}}, nil,
			)(failInner),
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/v1/price?asset=native&quote=fiat:USD", nil)
				sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K1", MonthlyQuota: 100}
				return r.WithContext(auth.WithSubject(r.Context(), sub))
			},
			wantStatus: http.StatusTooManyRequests,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// CacheControl outermost, exactly as in server.go — it
			// pre-sets the public route directive the rejection
			// writer must override.
			h := middleware.CacheControl(tc.handler)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, tc.req())

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store — a shared cache may store this %d against the public URL key", cc, w.Code)
			}
		})
	}
}
