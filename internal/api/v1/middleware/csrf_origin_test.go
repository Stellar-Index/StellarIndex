// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sameSiteRig wraps a handler that records whether it ran, behind the
// guard. `corsOrigins` non-nil also chains the real CORS middleware so
// the allow-list hand-off is exercised end to end rather than faked.
func sameSiteRig(corsOrigins []string) (http.Handler, *bool) {
	reached := new(bool)
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusNoContent)
	})
	h = middleware.RequireSameSiteWrite(quietLogger())(h)
	if corsOrigins != nil {
		opts := middleware.CORSOptions{
			AllowedOrigins:   corsOrigins,
			AllowCredentials: true,
			AllowedMethods:   []string{"GET", "POST", "DELETE", "OPTIONS"},
		}
		// CORS panics on wildcard+credentials (no browser honours the
		// combo), so the wildcard rig has to be the un-credentialed
		// public-read-API shape a real deployment would use.
		if len(corsOrigins) == 1 && corsOrigins[0] == "*" {
			opts.AllowCredentials = false
		}
		h = middleware.CORS(opts)(h)
	}
	return h, reached
}

func TestRequireSameSiteWrite_BlocksCrossSitePost(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/dashboard/keys", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if *reached {
		t.Fatal("handler ran for a cross-site write")
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got := body["type"]; got != "https://api.stellarindex.io/errors/cross-site-request-blocked" {
		t.Errorf("problem type = %v", got)
	}
}

func TestRequireSameSiteWrite_AllowsAllowListedOrigin(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/dashboard/keys", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Origin", "https://stellarindex.io")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent || !*reached {
		t.Fatalf("status = %d reached = %v, want 204/true (the explorer is allow-listed)", w.Code, *reached)
	}
}

func TestRequireSameSiteWrite_AllowsSameOriginWithoutCORS(t *testing.T) {
	// No CORS middleware at all: a deployment serving the dashboard
	// from the API's own origin must still work.
	h, reached := sameSiteRig(nil)

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/dashboard/keys", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Origin", "https://api.stellarindex.io")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent || !*reached {
		t.Fatalf("status = %d reached = %v, want 204/true (same-origin write)", w.Code, *reached)
	}
}

func TestRequireSameSiteWrite_BlocksWhenNoOriginOrReferer(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	req := httptest.NewRequest(http.MethodDelete, "https://api.stellarindex.io/v1/dashboard/keys/x", nil)
	req.Host = "api.stellarindex.io"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (fail closed with neither header)", w.Code)
	}
	if *reached {
		t.Fatal("handler ran for a headerless write")
	}
}

func TestRequireSameSiteWrite_NullOriginFallsToRefererAndIsBlocked(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/auth/verify-code", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Origin", "null") // sandboxed iframe / opaque origin
	req.Header.Set("Referer", "https://evil.example/csrf")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Origin: null must not pass)", w.Code)
	}
	if *reached {
		t.Fatal("handler ran for an opaque-origin write")
	}
}

func TestRequireSameSiteWrite_RefererOnlyFromAllowListedSitePasses(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/auth/login", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Referer", "https://stellarindex.io/signin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent || !*reached {
		t.Fatalf("status = %d reached = %v, want 204/true", w.Code, *reached)
	}
}

func TestRequireSameSiteWrite_SafeMethodsUntouched(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	for _, m := range []string{http.MethodGet, http.MethodHead} {
		*reached = false
		req := httptest.NewRequest(m, "https://api.stellarindex.io/v1/dashboard/keys", nil)
		req.Host = "api.stellarindex.io"
		req.Header.Set("Origin", "https://evil.example")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent || !*reached {
			t.Errorf("%s: status = %d reached = %v, want 204/true (reads are not gated)", m, w.Code, *reached)
		}
	}
}

// TestRequireSameSiteWrite_WildcardCORSDoesNotOpenTheDashboard — a
// deployment that serves a public read API with `AllowedOrigins:["*"]`
// must not thereby let every site on the internet drive a cookie-
// authenticated write.
func TestRequireSameSiteWrite_WildcardCORSDoesNotOpenTheDashboard(t *testing.T) {
	h, reached := sameSiteRig([]string{"*"})

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/dashboard/keys", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (wildcard CORS is not a credentialed allow-list)", w.Code)
	}
	if *reached {
		t.Fatal("handler ran under a wildcard CORS policy")
	}
}

// TestRequireSameSiteWrite_SiblingSubdomainIsBlocked is the gap
// SameSite=Lax structurally cannot close: `evil.stellarindex.io` is
// SAME-SITE with `api.stellarindex.io`, so Lax sends the session
// cookie. Only an origin allow-list rejects it.
func TestRequireSameSiteWrite_SiblingSubdomainIsBlocked(t *testing.T) {
	h, reached := sameSiteRig([]string{"https://stellarindex.io"})

	req := httptest.NewRequest(http.MethodPost, "https://api.stellarindex.io/v1/dashboard/webhooks", nil)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Origin", "https://uploads.stellarindex.io")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (same-site sibling origin is still not allow-listed)", w.Code)
	}
	if *reached {
		t.Fatal("handler ran for a same-site sibling origin")
	}
}
