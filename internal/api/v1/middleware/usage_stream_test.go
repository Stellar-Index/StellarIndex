package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

func streamUsageTotal(t *testing.T, counter *usage.Counter, subject auth.Subject) int64 {
	t.Helper()
	// The stream-open counter write runs on the shared after-response
	// pool, not inline, so a read right after the headers commit
	// must wait for it to land first.
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}
	days, err := counter.Read(context.Background(), middleware.UsageKeyForSubject(subject), 3)
	if err != nil {
		t.Fatalf("counter.Read: %v", err)
	}
	var total int64
	for _, d := range days {
		total += d.Requests
	}
	return total
}

// runOpenStream serves one SSE request whose handler commits status, then
// blocks until the returned release func is called. It returns once the
// headers are committed; wait blocks until the handler has returned.
func runOpenStream(t *testing.T, counter *usage.Counter, subject auth.Subject, status int) (release, wait func()) {
	t.Helper()
	opened := make(chan struct{})
	unblock := make(chan struct{})
	done := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/price/tip/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(opened)
		<-unblock
	})
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}
	h := middleware.Chain(mux, stamp, middleware.UsageTracker(counter, nil))
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/price/tip/stream", nil))
	}()
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("stream handler never committed its headers")
	}
	var released bool
	release = func() {
		if !released {
			released = true
			close(unblock)
		}
	}
	t.Cleanup(release)
	wait = func() {
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("stream handler never returned")
		}
	}
	return release, wait
}
