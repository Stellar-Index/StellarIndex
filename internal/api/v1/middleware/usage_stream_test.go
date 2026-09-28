package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

func streamUsageTotal(t *testing.T, counter *usage.Counter, subject auth.Subject) int64 {
	t.Helper()
	// The stream-open counter write runs on the shared after-response
	// pool, not inline (GH-627), so a read right after the headers commit
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

// TestUsageTracker_StreamCountedAtOpen pins GH-1279: an SSE stream is
// exempt from RequestTimeout, so it can stay open across a day boundary or
// be killed by a deploy without ever returning. Counting it only at close
// billed it on the wrong day or not at all. It must be counted once, when
// the stream opens, and not again when it closes.
func TestUsageTracker_StreamCountedAtOpen(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	var now atomic.Int64
	now.Store(time.Date(2026, 9, 27, 23, 59, 0, 0, time.UTC).Unix())
	counter := usage.New(rdb, usage.WithClock(func() time.Time { return time.Unix(now.Load(), 0) }))
	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_stream", Identifier: "acct:stream"}

	_, wait := runOpenStream(t, counter, subject, http.StatusOK)

	if got := streamUsageTotal(t, counter, subject); got != 1 {
		t.Fatalf("usage total while stream is open = %d, want 1 — an open SSE stream must be "+
			"metered when it opens, not only when (and if) it closes", got)
	}
	now.Store(time.Date(2026, 9, 28, 0, 30, 0, 0, time.UTC).Unix())
	wait()

	days, err := counter.Read(context.Background(), middleware.UsageKeyForSubject(subject), 3)
	if err != nil {
		t.Fatalf("counter.Read: %v", err)
	}
	perDay := map[string]int64{}
	for _, d := range days {
		perDay[d.Date] = d.Requests
	}
	if perDay["2026-09-27"] != 1 || perDay["2026-09-28"] != 0 {
		t.Errorf("per-day usage = %v, want exactly 1 on the open day 2026-09-27 and 0 on the "+
			"close day — a stream is one request, counted once when it opens", perDay)
	}
}

// TestUsageTracker_StreamRefusedAtOpenNotBilled guards that metering at open
// still classifies by the committed status: a stream refused with 429 must
// not eat monthly quota (COR-05), while the detail family still sees it.
func TestUsageTracker_StreamRefusedAtOpenNotBilled(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)
	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_stream429", Identifier: "acct:stream429"}

	_, wait := runOpenStream(t, counter, subject, http.StatusTooManyRequests)
	wait()

	if got := streamUsageTotal(t, counter, subject); got != 0 {
		t.Errorf("billable usage total for a 429-refused stream = %d, want 0", got)
	}
	rows, err := counter.ScanDetail(context.Background(), []string{time.Now().UTC().Format("2006-01-02")})
	if err != nil {
		t.Fatalf("ScanDetail: %v", err)
	}
	var throttled int64
	for _, r := range rows {
		if r.Class == usage.ClassThrottled {
			throttled += r.Count
		}
	}
	if throttled != 1 {
		t.Errorf("throttled detail count = %d, want 1 (rows %+v)", throttled, rows)
	}
}
