package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TrailingSlashRedirect happy path: /v1/assets/native/ → 308 Location
// /v1/assets/native (preserves query string), and the inner handler
// is NOT called.

func TestTrailingSlashRedirect_redirectsAndSkipsHandler(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	mw := TrailingSlashRedirect(inner)

	req := httptest.NewRequest(http.MethodGet, "/v1/assets/native/", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want 308", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/assets/native" {
		t.Errorf("Location = %q, want /v1/assets/native", loc)
	}
	if called {
		t.Error("inner handler should not have been called")
	}
}

func TestTrailingSlashRedirect_preservesQueryString(t *testing.T) {
	mw := TrailingSlashRedirect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/assets/?cursor=abc&limit=10", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want 308", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/assets?cursor=abc&limit=10" {
		t.Errorf("Location = %q, want /v1/assets?cursor=abc&limit=10", loc)
	}
}

func TestTrailingSlashRedirect_rootIsExempt(t *testing.T) {
	// "/" must not redirect to "" — that would be a broken loop.
	called := false
	mw := TrailingSlashRedirect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (default)", rec.Code)
	}
	if !called {
		t.Error("inner handler should have been called for root")
	}
}

func TestTrailingSlashRedirect_noSlashPassesThrough(t *testing.T) {
	called := false
	mw := TrailingSlashRedirect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if !called {
		t.Error("inner handler should have been called for no-slash path")
	}
}

// TestTrailingSlashRedirect_refusesProtocolRelativeTarget is the
// SEC-16 regression: a request path of "//evil.com/" must NOT redirect
// to "//evil.com" (a protocol-relative Location a browser resolves as
// an absolute redirect off the API origin — an unauthenticated open
// redirect). The middleware must instead fall through to next (which
// 404s / lets the mux apply its own safe same-origin cleanup) rather
// than ever emitting that Location.
func TestTrailingSlashRedirect_refusesProtocolRelativeTarget(t *testing.T) {
	called := false
	mw := TrailingSlashRedirect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNotFound)
	}))

	req := httptest.NewRequest(http.MethodGet, "//evil.com/", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if loc := rec.Header().Get("Location"); strings.HasPrefix(loc, "//") {
		t.Fatalf("open redirect: Location = %q resolves off the API origin (SEC-16)", loc)
	}
	if rec.Code == http.StatusPermanentRedirect {
		t.Errorf("status = 308 — middleware redirected to a protocol-relative target instead of refusing")
	}
	if !called {
		t.Error("expected the request to fall through to next (no safe redirect target)")
	}
}

// 308 (rather than 301/302) preserves method and body for POST/DELETE.
// Pin the redirect status itself so a refactor can't silently weaken
// the redirect to a method-changing 301.
func TestTrailingSlashRedirect_methodAgnostic(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			mw := TrailingSlashRedirect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("inner handler should not have been called")
			}))
			req := httptest.NewRequest(method, "/v1/account/keys/", nil)
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)

			if rec.Code != http.StatusPermanentRedirect {
				t.Errorf("status = %d, want 308 for %s", rec.Code, method)
			}
			if loc := rec.Header().Get("Location"); loc != "/v1/account/keys" {
				t.Errorf("Location = %q, want /v1/account/keys", loc)
			}
		})
	}
}
