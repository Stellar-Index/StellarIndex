package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRequestTimeout_SetsDeadline pins the core wiring (C3-1/C3-2/P1,
// audit-2026-07-16): a non-streaming request reaches the handler with a
// context deadline roughly d out, so every handler inherits a bound even
// when it forgets its own.
func TestRequestTimeout_SetsDeadline(t *testing.T) {
	const d = 5 * time.Second
	var (
		gotDeadline bool
		remaining   time.Duration
	)
	h := RequestTimeout(d)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		dl, ok := r.Context().Deadline()
		gotDeadline = ok
		if ok {
			remaining = time.Until(dl)
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/assets/native/holders", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !gotDeadline {
		t.Fatal("handler context had no deadline; RequestTimeout did not bound the request")
	}
	// The deadline must be in the future and no further out than d — a
	// generous lower bound tolerates test-machine scheduling jitter.
	if remaining <= 0 || remaining > d {
		t.Errorf("deadline remaining = %v, want in (0, %v]", remaining, d)
	}
}

// TestRequestTimeout_ExemptsStreamingPaths guards the SSE carve-out: a
// blanket deadline would sever a long-lived stream mid-flight, so every
// /stream-suffixed path must pass through with the parent's (unbounded)
// context.
func TestRequestTimeout_ExemptsStreamingPaths(t *testing.T) {
	streamPaths := []string{
		"/v1/ledger/stream",
		"/v1/price/stream",
		"/v1/price/tip/stream",
		"/v1/observations/stream",
	}
	for _, p := range streamPaths {
		var hadDeadline bool
		h := RequestTimeout(5 * time.Second)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			_, hadDeadline = r.Context().Deadline()
		}))
		req := httptest.NewRequest(http.MethodGet, p, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
		if hadDeadline {
			t.Errorf("%s: streaming path must NOT get a request deadline", p)
		}
	}
}

// TestRequestTimeout_DisabledWhenNonPositive confirms d <= 0 is a no-op:
// the handler runs against the parent context unchanged.
func TestRequestTimeout_DisabledWhenNonPositive(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		var hadDeadline bool
		h := RequestTimeout(d)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			_, hadDeadline = r.Context().Deadline()
		}))
		req := httptest.NewRequest(http.MethodGet, "/v1/vwap", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
		if hadDeadline {
			t.Errorf("d=%v: disabled middleware must not inject a deadline", d)
		}
	}
}

func TestIsStreamingPath(t *testing.T) {
	cases := map[string]bool{
		"/v1/price/stream":          true,
		"/v1/observations/stream":   true,
		"/v1/vwap":                  false,
		"/v1/assets/native/holders": false,
		"/v1/streamers":             false, // suffix is "/stream", not substring
	}
	for path, want := range cases {
		if got := isStreamingPath(path); got != want {
			t.Errorf("isStreamingPath(%q) = %v, want %v", path, got, want)
		}
	}
}
