package middleware_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// TestUsageTracker_RecordsAfterClientAbort pins the post-response half of
// C3-102 (audit-2026-07-23).
//
// UsageTracker's counters run AFTER the response is flushed, but they used
// `r.Context()` — which is already cancelled when the client aborted, or
// when the handler consumed the whole RequestTimeout budget. go-redis
// honours context cancellation, so the write failed and was swallowed at
// Debug level. The legacy total is the MONTHLY-QUOTA INPUT, so a lost row
// is lost billing signal, and moving RequestTimeout outward (the other half
// of C3-102) would have widened the window.
//
// The fix derives the write context with context.WithoutCancel plus its own
// bound. This test drives the REAL middleware with an already-cancelled
// request context and asserts the counter still recorded.
func TestUsageTracker_RecordsAfterClientAbort(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)

	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_abort", Identifier: "acct:abort"}

	// The handler writes its response and THEN the request is aborted —
	// exactly the served-then-abandoned shape.
	var cancel context.CancelFunc
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ohlc", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		cancel() // client goes away / RequestTimeout budget exhausted
	})

	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}
	h := middleware.Chain(mux, stamp, middleware.UsageTracker(counter, nil))

	ctx, c := context.WithCancel(context.Background())
	cancel = c
	req := httptest.NewRequest(http.MethodGet, "/v1/ohlc", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	days, err := counter.Read(context.Background(), "key:kid_abort", 3)
	if err != nil {
		t.Fatalf("counter.Read: %v", err)
	}
	var total int64
	for _, d := range days {
		total += d.Requests
	}
	if total != 1 {
		t.Errorf("legacy usage total = %d, want 1 — a request whose context was cancelled "+
			"after the response was served must still be counted; the counter is the "+
			"monthly-quota input, so dropping it is lost billing signal AND a quota-evasion "+
			"vector (abort before the body completes)", total)
	}
}

// TestTouchUsage_TouchesAfterClientAbort is the sibling assertion for the
// other post-response writer: `last_used` bookkeeping must survive an
// aborted request for the same reason.
func TestTouchUsage_TouchesAfterClientAbort(t *testing.T) {
	toucher := &recordingToucher{}
	debouncer := alwaysTouch{}
	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_touch"}

	var cancel context.CancelFunc
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ohlc", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		cancel()
	})
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}
	h := middleware.Chain(mux, stamp, middleware.TouchUsage(toucher, debouncer, nil))

	ctx, c := context.WithCancel(context.Background())
	cancel = c
	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/v1/ohlc", nil).WithContext(ctx))

	if toucher.calls != 1 {
		t.Errorf("TouchUsage called %d times, want 1 — post-response bookkeeping must not "+
			"inherit the request's cancellation", toucher.calls)
	}
}

type recordingToucher struct{ calls int }

func (r *recordingToucher) TouchUsage(ctx context.Context, _ string, _ net.IP, _ string) error {
	if err := ctx.Err(); err != nil {
		return err // a cancelled ctx must not reach here
	}
	r.calls++
	return nil
}

type alwaysTouch struct{}

func (alwaysTouch) ShouldTouch(ctx context.Context, _ string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, nil
}

// TestUsageTracker_AbortedRequestConsumesQuota pins the DECISION that came
// with the C3-102 post-response fix, so it cannot be silently reverted as
// "an unintended side effect".
//
// Before the fix an aborted request's Increment died on the cancelled
// context, so the request was free. After it, the request counts against the
// monthly quota. That is intended: statusRecorder defaults to 200, a
// served-then-abandoned request classes as billable, and it really did
// consume the read, the pool connection and the CPU. Not counting it is a
// quota-evasion vector — abort before the body completes and get unmetered
// traffic indefinitely.
func TestUsageTracker_AbortedRequestConsumesQuota(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)

	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_evade", Identifier: "acct:evade"}
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}

	const attempts = 5
	for i := 0; i < attempts; i++ {
		var cancel context.CancelFunc
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/ohlc", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			cancel()
		})
		h := middleware.Chain(mux, stamp, middleware.UsageTracker(counter, nil))
		ctx, c := context.WithCancel(context.Background())
		cancel = c
		h.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/v1/ohlc", nil).WithContext(ctx))
	}

	mtd, err := counter.MonthToDate(context.Background(), "key:kid_evade")
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if mtd != attempts {
		t.Errorf("month-to-date = %d after %d aborted requests, want %d — aborting before "+
			"the body completes must NOT buy unmetered traffic", mtd, attempts, attempts)
	}
}
