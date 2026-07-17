package obs

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
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
// distribution); HTTPRequestSuccessDuration stays untouched.
//
// This drives the REAL middleware chain (HTTPMetrics → CaptureRoute →
// ServeMux) rather than hand-observing the histograms, and asserts on
// before/after deltas of the actual counters so it fails if the
// middleware ever regresses the 5xx-vs-success split.
func TestHTTPMetrics_Fast5xxDoesNotCountAsSuccess(t *testing.T) {
	const (
		method = "GET"
		route  = "/test-f0105"
	)

	// Route through a real ServeMux so r.Pattern (and thus the `route`
	// label) is populated exactly as it is in production. Handler
	// returns a fast 500.
	mux := http.NewServeMux()
	mux.HandleFunc(method+" "+route, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h := HTTPMetrics(CaptureRoute(mux))

	successBefore := histSampleCount(t, HTTPRequestSuccessDuration.WithLabelValues(method, route))
	durationBefore := histSampleCount(t, HTTPRequestDuration.WithLabelValues(method, route))
	total5xxBefore := testutil.ToFloat64(HTTPRequestsTotal.WithLabelValues(method, route, "500"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, route, nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("handler status = %d, want 500", rec.Code)
	}

	successAfter := histSampleCount(t, HTTPRequestSuccessDuration.WithLabelValues(method, route))
	durationAfter := histSampleCount(t, HTTPRequestDuration.WithLabelValues(method, route))
	total5xxAfter := testutil.ToFloat64(HTTPRequestsTotal.WithLabelValues(method, route, "500"))

	// The 5xx counter must tick — the request WAS served and WAS a 500.
	if got := total5xxAfter - total5xxBefore; got != 1 {
		t.Errorf("http_requests_total{status=\"500\"} delta = %v, want 1", got)
	}
	// The full-distribution latency histogram must observe the 500...
	if got := durationAfter - durationBefore; got != 1 {
		t.Errorf("http_request_duration_seconds count delta = %d, want 1", got)
	}
	// ...but the success-only histogram must NOT (F-0105: a fast 5xx is
	// not a latency success and must stay out of the SLO numerator).
	if got := successAfter - successBefore; got != 0 {
		t.Errorf("fast 500 leaked into success histogram: count delta = %d, want 0", got)
	}
}

// histSampleCount reads the current observation count off a single
// histogram child (the value behind `<name>_count` on a scrape).
func histSampleCount(t *testing.T, o prometheus.Observer) uint64 {
	t.Helper()
	m, ok := o.(prometheus.Metric)
	if !ok {
		t.Fatalf("observer %T is not a prometheus.Metric", o)
	}
	var dm dto.Metric
	if err := m.Write(&dm); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return dm.GetHistogram().GetSampleCount()
}
