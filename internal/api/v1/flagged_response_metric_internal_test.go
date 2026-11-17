package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// A degraded or stale 200 is invisible to the 5xx SLO, so every write path must reach the
// per-route counter, including through an inner wrapping middleware (ETag buffers the body).
func TestFlaggedResponses_countedByRoutePattern(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/flagtest/envelope/{id}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{}, Flags{Degraded: true, Stale: true})
	})
	mux.HandleFunc("GET /v1/flagtest/clean/{id}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{}, Flags{})
	})
	mux.HandleFunc("GET /v1/flagtest/detail/{id}", func(w http.ResponseWriter, _ *http.Request) {
		writeCachedAssetDetail(w, &assetDetailEntry{body: []byte("{}"), cachedAt: time.Now(), degraded: true})
	})
	mux.HandleFunc("GET /v1/flagtest/list/{id}", func(w http.ResponseWriter, _ *http.Request) {
		writeCachedAssetList(w, []byte("{}"), true)
	})
	h := obs.HTTPMetrics(middleware.ETag(obs.CaptureRoute(mux)))

	count := func(route, flag string) float64 {
		return testutil.ToFloat64(obs.APIFlaggedResponsesTotal.WithLabelValues(route, flag))
	}
	cases := []struct {
		path, route     string
		degraded, stale float64
	}{
		{"/v1/flagtest/envelope/a", "/v1/flagtest/envelope/{id}", 1, 1},
		{"/v1/flagtest/clean/a", "/v1/flagtest/clean/{id}", 0, 0},
		{"/v1/flagtest/detail/a", "/v1/flagtest/detail/{id}", 1, 0},
		{"/v1/flagtest/list/a", "/v1/flagtest/list/{id}", 0, 1},
	}
	for _, tc := range cases {
		d0, s0 := count(tc.route, "degraded"), count(tc.route, "stale")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.path, rec.Code)
		}
		if got := count(tc.route, "degraded") - d0; got != tc.degraded {
			t.Errorf("%s: degraded delta = %v, want %v", tc.path, got, tc.degraded)
		}
		if got := count(tc.route, "stale") - s0; got != tc.stale {
			t.Errorf("%s: stale delta = %v, want %v", tc.path, got, tc.stale)
		}
	}
	if got := count("/v1/flagtest/envelope/a", "degraded"); got != 0 {
		t.Errorf("raw path leaked into the route label: %v", got)
	}
}

// A degraded response behind the idempotency recorder must still reach the counter.
func TestFlaggedResponses_countedThroughIdempotency(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/flagtest/idem/{id}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{}, Flags{Degraded: true})
	})
	idem := middleware.Idempotency(middleware.NewIdempotencyStore(time.Minute), func(*http.Request) string { return "acct-1" })
	h := obs.HTTPMetrics(idem(obs.CaptureRoute(mux)))
	const route = "/v1/flagtest/idem/{id}"
	before := testutil.ToFloat64(obs.APIFlaggedResponsesTotal.WithLabelValues(route, "degraded"))
	req := httptest.NewRequest(http.MethodPost, "/v1/flagtest/idem/a", nil)
	req.Header.Set(middleware.IdempotencyKeyHeader, "k1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := testutil.ToFloat64(obs.APIFlaggedResponsesTotal.WithLabelValues(route, "degraded")) - before; got != 1 {
		t.Errorf("degraded delta = %v, want 1", got)
	}
}
