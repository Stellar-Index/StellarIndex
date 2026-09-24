package v1_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// testServerImpl wraps httptest.Server so asset + server tests
// share one construction path.
type testServerImpl struct {
	URL string
	ts  *httptest.Server
}

func startHTTPTest(t *testing.T, h http.Handler) *testServerImpl {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &testServerImpl{URL: ts.URL, ts: ts}
}

// remainingBeforeProbe reads the caller's rate-limit remainder through
// probeURL — a request that costs exactly the base token and is answered
// 400 no-store — and returns it as it stood before the probe's own token.
// A shared-cacheable 200 carries no X-RateLimit-* (a CDN would replay
// them to other callers), so a charge made by one is observed here. It
// first asserts that the cacheable response really does carry none.
func remainingBeforeProbe(t *testing.T, cacheable *http.Response, probeURL string) int {
	t.Helper()
	if got := cacheable.Header.Get("X-RateLimit-Remaining"); got != "" {
		t.Fatalf("shared-cacheable response (Cache-Control %q) carries X-RateLimit-Remaining %q",
			cacheable.Header.Get("Cache-Control"), got)
	}
	probe := mustGet(t, probeURL)
	if probe.StatusCode != http.StatusBadRequest || probe.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("probe: status %d Cache-Control %q, want 400 no-store",
			probe.StatusCode, probe.Header.Get("Cache-Control"))
	}
	n, err := strconv.Atoi(probe.Header.Get("X-RateLimit-Remaining"))
	if err != nil {
		t.Fatalf("probe X-RateLimit-Remaining: %v", err)
	}
	return n + 1
}
