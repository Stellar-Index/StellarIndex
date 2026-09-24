package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

var perCallerHeaderNames = []string{
	"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", middleware.HeaderRequestID,
}

// perCallerChain mirrors the production order (Server.Handler): RequestID
// outermost, CacheControl, the limiter innermost, so the headers under
// test are set by the real writers rather than by the fixture.
func perCallerChain(t *testing.T, limit int, inner http.Handler) http.Handler {
	t.Helper()
	rdb, _ := newRLRedis(t)
	limited := middleware.RateLimit(ratelimit.New(rdb, limit, time.Minute), nil, nil, nil)(inner)
	return middleware.RequestID(middleware.CacheControlWithCDN(true)(limited))
}

func serve(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// A shared cache replays a `public, s-maxage` response to every later
// caller, so it must not carry one caller's rate-limit account or
// request id: the next caller would be shown a budget that is not theirs
// and was never charged for the cached hit.
func TestCacheControl_SharedCacheableResponseCarriesNoPerCallerHeaders(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		inner http.HandlerFunc
	}{
		{"route band, explicit WriteHeader", "/v1/markets", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		}},
		{"route band, implicit 200 on Write", "/v1/assets", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[]}`))
		}},
		// A handler override is decided after the middleware ran; the
		// strip must read the directive that actually leaves.
		{"handler override to public", "/v1/ledger/tip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=2")
			w.WriteHeader(http.StatusOK)
		}},
		{"not-modified revalidation", "/v1/markets", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotModified)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				rec := serve(perCallerChain(t, 100, tc.inner), method, tc.path)
				for _, name := range perCallerHeaderNames {
					if got := rec.Header().Values(name); len(got) != 0 {
						t.Errorf("%s %s: %s = %q on a shared-cacheable response (Cache-Control %q)",
							method, tc.path, name, got, rec.Header().Get("Cache-Control"))
					}
				}
				if cc := rec.Header().Get("Cache-Control"); cc == "" {
					t.Errorf("%s %s: Cache-Control was dropped", method, tc.path)
				}
			}
		})
	}
}

// The strip is scoped to responses a shared cache may reuse without the
// origin. Everything else keeps the headers, with the values the limiter
// and RequestID wrote.
func TestCacheControl_PerCallerHeadersKeptWhereNoSharedReuse(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	cases := []struct {
		name, method, path string
		inner              http.HandlerFunc
	}{
		{"private route", http.MethodGet, "/v1/price/tip", ok},
		{"default private, no-store", http.MethodGet, "/v1/account/usage", ok},
		// POST responses are not stored without Content-Location (RFC 9110 §9.3.3).
		{"POST on a public band", http.MethodPost, "/v1/price/batch", ok},
		{"problem writer overrides to no-store", http.MethodGet, "/v1/markets", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusBadRequest)
		}},
		// SSE: `no-cache` forces revalidation, so the headers seen are
		// always the current request's. Flush must still reach the writer.
		{"event stream no-cache", http.MethodGet, "/v1/ledger/stream", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
			f, isFlusher := w.(http.Flusher)
			if !isFlusher {
				t.Error("stripper hid http.Flusher from the stream handler")
				return
			}
			f.Flush()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(perCallerChain(t, 100, tc.inner), tc.method, tc.path)
			if got := rec.Header().Get("X-RateLimit-Limit"); got != "100" {
				t.Errorf("X-RateLimit-Limit = %q, want 100", got)
			}
			if got := rec.Header().Get("X-RateLimit-Remaining"); got != "99" {
				t.Errorf("X-RateLimit-Remaining = %q, want 99", got)
			}
			if rec.Header().Get("X-RateLimit-Reset") == "" {
				t.Error("X-RateLimit-Reset missing")
			}
			if rec.Header().Get(middleware.HeaderRequestID) == "" {
				t.Error("X-Request-ID missing")
			}
		})
	}
}

// A 429 on a public band is written no-store by the limiter, so the
// caller still gets the account and Retry-After it needs to back off.
func TestCacheControl_RateLimitDenialOnPublicBandKeepsHeaders(t *testing.T) {
	h := perCallerChain(t, 1, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if rec := serve(h, http.MethodGet, "/v1/markets"); rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", rec.Code)
	}
	rec := serve(h, http.MethodGet, "/v1/markets")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("429 X-RateLimit-Remaining = %q, want 0", got)
	}
	if rec.Header().Get("Retry-After") == "" || rec.Header().Get(middleware.HeaderRequestID) == "" {
		t.Errorf("429 lost Retry-After or X-Request-ID: %v", rec.Header())
	}
}
