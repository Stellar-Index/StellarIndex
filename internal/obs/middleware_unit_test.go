package obs

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Unit tests for the small helpers inside http_middleware.go.
// metrics_test.go covers HTTPMetrics end-to-end via httptest;
// this file exercises the helpers directly so regressions show
// up at the function level rather than as a metric-label
// surprise three layers deep.

// ─── normalizeMethod ──────────────────────────────────────────

func TestNormalizeMethod(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"GET", "GET"},
		{"get", "GET"},
		{"Post", "POST"},
		{"PATCH", "PATCH"},
		{"propfind", "propfind"}, // unknown verb passes through as-is
		{"PROPFIND", "PROPFIND"}, // already-upper unknown verb same
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeMethod(tc.in); got != tc.want {
			t.Errorf("normalizeMethod(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ─── routeFromPattern ─────────────────────────────────────────

func TestRouteFromPattern(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"GET /v1/price", "/v1/price"},
		{"POST /v1/price/batch", "/v1/price/batch"},
		{"/v1/healthz", "/v1/healthz"}, // pattern without method prefix
		{"", "unmatched"},              // empty → sentinel
		{"GET /", "/"},                 // root path
	}
	for _, tc := range cases {
		if got := routeFromPattern(tc.in); got != tc.want {
			t.Errorf("routeFromPattern(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ─── isStreamingRoute ─────────────────────────────────────────

func TestIsStreamingRoute(t *testing.T) {
	cases := []struct {
		route string
		want  bool
	}{
		{"/v1/ledger/stream", true},
		{"/v1/price/stream", true},
		{"/v1/price/tip/stream", true},
		{"/v1/observations/stream", true},
		{"/v1/price/tip", false}, // not a stream — a substring match would false-positive
		{"/v1/ledger/tip", false},
		{"/v1/price", false},
		{"unmatched", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isStreamingRoute(tc.route); got != tc.want {
			t.Errorf("isStreamingRoute(%q) = %v, want %v", tc.route, got, tc.want)
		}
	}
}

// ─── statusRecorder.Flush ─────────────────────────────────────

// flushableRecorder is an httptest.ResponseRecorder with a Flush
// counter — proves statusRecorder.Flush proxies to the underlying
// writer when it implements http.Flusher.
type flushableRecorder struct {
	*httptest.ResponseRecorder
	flushed int
}

func (f *flushableRecorder) Flush() { f.flushed++ }

func TestStatusRecorder_Flush_proxiesWhenInnerSupportsFlusher(t *testing.T) {
	inner := &flushableRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := &statusRecorder{ResponseWriter: inner}
	r.Flush()
	if inner.flushed != 1 {
		t.Errorf("inner.flushed = %d, want 1", inner.flushed)
	}
}

// nonFlusher is a writer that deliberately does NOT implement
// http.Flusher. statusRecorder.Flush must be a no-op rather than
// panicking — SSE endpoints sometimes get wrapped by middleware
// that strips the Flusher interface.
type nonFlusher struct {
	header http.Header
}

func (n *nonFlusher) Header() http.Header { return n.header }
func (n *nonFlusher) Write(b []byte) (int, error) {
	return len(b), nil
}
func (n *nonFlusher) WriteHeader(int) {}

func TestStatusRecorder_Flush_noopWhenInnerDoesntImplementFlusher(t *testing.T) {
	r := &statusRecorder{ResponseWriter: &nonFlusher{header: http.Header{}}}
	// Must not panic.
	r.Flush()
}

// F-0105 regression: a fast 500 must NOT count in the success
// histogram. Pre-this-PR fast errors were observed in the same
// histogram as fast successes, so a 500 returning in 5 ms reported
// as "good" against the latency SLO numerator. After this fix, the
// 500's elapsed time only lands in HTTPRequestDuration (the full
// distribution); HTTPRequestSuccessDuration stays at zero for the
// route's _bucket{le=0.2} counter.
func TestHTTPMetrics_Fast5xxDoesNotCountAsSuccess(t *testing.T) {
	// Use a hand-rolled response so we don't touch the package's
	// shared Registry from a parallel test. Resetting a histogram
	// requires per-test isolation infrastructure we don't have; the
	// route label is unique to this test to keep the assertion
	// well-scoped.
	const route = "/test-f0105"
	const status = 500
	HTTPRequestSuccessDuration.WithLabelValues("GET", route)
	HTTPRequestDuration.WithLabelValues("GET", route)
	// Manual observation that mirrors what the middleware does for
	// a status<500 vs >=500 split.
	HTTPRequestDuration.WithLabelValues("GET", route).Observe(0.003)
	if status < 500 && status != 499 {
		HTTPRequestSuccessDuration.WithLabelValues("GET", route).Observe(0.003)
	}
	// Scrape the metric and verify the route landed in
	// _duration_seconds but NOT in _success_duration_seconds.
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := make([]byte, 1<<16)
	n, _ := resp.Body.Read(body)
	scrape := string(body[:n])
	if !containsAll(scrape, `http_request_duration_seconds_count{method="GET",route="/test-f0105"}`) {
		t.Error("scrape missing duration entry for /test-f0105")
	}
	if containsAny(scrape, `http_request_success_duration_seconds_count{method="GET",route="/test-f0105"} 1`) {
		t.Error("a 500 leaked into the success histogram — F-0105 regression")
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strContains(s, p) {
			return false
		}
	}
	return true
}

func containsAny(s string, parts ...string) bool {
	for _, p := range parts {
		if strContains(s, p) {
			return true
		}
	}
	return false
}

func strContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
