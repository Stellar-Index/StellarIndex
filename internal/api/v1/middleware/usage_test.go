package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// usageTestStack builds mux(pattern → status handler) wrapped by
// UsageTracker + a subject-stamping shim, backed by miniredis.
// Returns the server + the counter for read-back assertions.
func usageTestStack(t *testing.T, subject auth.Subject, pattern string, status int) (*httptest.Server, *usage.Counter) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)

	mux := http.NewServeMux()
	mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	})

	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if subject.Tier != "" {
				r = r.WithContext(auth.WithSubject(r.Context(), subject))
			}
			next.ServeHTTP(w, r)
		})
	}

	h := middleware.Chain(mux, stamp, middleware.UsageTracker(counter, nil))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, counter
}

// lastTwoUTCDates returns today + yesterday (UTC) — the same window
// the rollup worker sweeps — so read-backs survive a midnight
// rollover between request and assertion.
func lastTwoUTCDates() []string {
	now := time.Now().UTC()
	return []string{
		now.AddDate(0, 0, -1).Format("2006-01-02"),
		now.Format("2006-01-02"),
	}
}

func detailCounts(t *testing.T, c *usage.Counter, subject string) map[[2]string]int64 {
	t.Helper()
	// Scan across a small date window so the assertion is immune to
	// a UTC midnight rollover between request and read-back.
	rows, err := c.ScanDetail(context.Background(), lastTwoUTCDates())
	if err != nil {
		t.Fatal(err)
	}
	out := map[[2]string]int64{}
	for _, r := range rows {
		if r.Subject != subject {
			continue
		}
		out[[2]string{r.Endpoint, r.Class}] += r.Count
	}
	return out
}

// TestUsageTracker_FamilyAndOutcome — a 200 on a patterned route
// records the ROUTE PATTERN (not the raw path) under class ok, and
// the legacy per-day total advances.
func TestUsageTracker_FamilyAndOutcome(t *testing.T) {
	ts, counter := usageTestStack(t, auth.Subject{
		Tier:  auth.TierAPIKey,
		KeyID: "kid_1",
	}, "GET /v1/assets/{asset_id}", http.StatusOK)

	for _, path := range []string{"/v1/assets/native", "/v1/assets/USDC-GA5Z"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}

	got := detailCounts(t, counter, "key:kid_1")
	if n := got[[2]string{"/v1/assets/{asset_id}", usage.ClassOK}]; n != 2 {
		t.Errorf("pattern-family ok count = %d, want 2 (counts = %v)", n, got)
	}
	for k := range got {
		if k[0] == "/v1/assets/native" || k[0] == "/v1/assets/USDC-GA5Z" {
			t.Errorf("raw path %q leaked into the endpoint label — cardinality bomb", k[0])
		}
	}

	days, err := counter.Read(context.Background(), "key:kid_1", 3)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, d := range days {
		total += d.Requests
	}
	if total != 2 {
		t.Errorf("legacy total = %d, want 2", total)
	}
}

// TestUsageTracker_OutcomeClasses — every status lands in its detail
// class, and only CALLER-caused outcomes advance the legacy billable
// total. wantBillable is 0 for 5xx: counting it as 1 would be
// exactly the defect (a platform failure charged to the
// customer's monthly quota) — see
// TestUsageTracker_ServerErrorExcludedFromLegacyTotal.
func TestUsageTracker_OutcomeClasses(t *testing.T) {
	cases := []struct {
		status       int
		class        string
		wantBillable int64
	}{
		{http.StatusNotFound, usage.ClassClientError, 1},
		{http.StatusInternalServerError, usage.ClassServerError, 0},
		{http.StatusNoContent, usage.ClassOK, 1},
	}
	for _, tc := range cases {
		ts, counter := usageTestStack(t, auth.Subject{
			Tier:  auth.TierAPIKey,
			KeyID: "kid_c",
		}, "GET /v1/thing", tc.status)
		resp, err := http.Get(ts.URL + "/v1/thing")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
			t.Fatal("after-response pool did not drain in time")
		}

		got := detailCounts(t, counter, "key:kid_c")
		if n := got[[2]string{"/v1/thing", tc.class}]; n != 1 {
			t.Errorf("status %d: class %q count = %d, want 1 (counts = %v)",
				tc.status, tc.class, n, got)
		}
		days, err := counter.Read(context.Background(), "key:kid_c", 3)
		if err != nil {
			t.Fatal(err)
		}
		var billable int64
		for _, d := range days {
			billable += d.Requests
		}
		if billable != tc.wantBillable {
			t.Errorf("status %d: legacy billable total = %d, want %d (days = %+v)",
				tc.status, billable, tc.wantBillable, days)
		}
	}
}

// TestUsageTracker_ServerErrorExcludedFromLegacyTotal — a 5xx records
// under the server-error class but must NOT advance the legacy per-day
// total.
//
// The legacy total is MonthlyQuota's input. Counting our own failures
// against it means a sustained outage burns the customer's paid
// monthly quota and then locks them out of their own plan for the rest
// of the month once we recover — we would charge them for the outage
// twice. Same reasoning the 429 exclusion already encodes
// (TestUsageTracker_ThrottledExcludedFromLegacyTotal), applied to the
// failure class the customer has even less control over.
//
// The request stays fully visible in the DETAIL family under the 5xx
// class, so the traffic is still observable and billable-vs-observed
// stay separable — only the quota input drops it.
func TestUsageTracker_ServerErrorExcludedFromLegacyTotal(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		ts, counter := usageTestStack(t, auth.Subject{
			Tier:  auth.TierAPIKey,
			KeyID: "kid_5",
		}, "GET /v1/price", status)

		resp, err := http.Get(ts.URL + "/v1/price")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
			t.Fatal("after-response pool did not drain in time")
		}

		got := detailCounts(t, counter, "key:kid_5")
		if n := got[[2]string{"/v1/price", usage.ClassServerError}]; n != 1 {
			t.Errorf("status %d: 5xx detail count = %d, want 1 — the request must stay observable (counts = %v)",
				status, n, got)
		}
		days, err := counter.Read(context.Background(), "key:kid_5", 3)
		if err != nil {
			t.Fatal(err)
		}
		var billable int64
		for _, d := range days {
			billable += d.Requests
		}
		if billable != 0 {
			t.Errorf("status %d: legacy billable total = %d, want 0 — a platform-caused failure must not eat monthly quota (days = %+v)",
				status, billable, days)
		}
	}
}

// TestUsageTracker_ThrottledExcludedFromLegacyTotal — a 429 records
// under the throttled class but must NOT advance the legacy per-day
// total (MonthlyQuota's input: rejected requests never eat quota).
func TestUsageTracker_ThrottledExcludedFromLegacyTotal(t *testing.T) {
	ts, counter := usageTestStack(t, auth.Subject{
		Tier:  auth.TierAPIKey,
		KeyID: "kid_t",
	}, "GET /v1/price", http.StatusTooManyRequests)

	resp, err := http.Get(ts.URL + "/v1/price")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}

	got := detailCounts(t, counter, "key:kid_t")
	if n := got[[2]string{"/v1/price", usage.ClassThrottled}]; n != 1 {
		t.Errorf("throttled count = %d, want 1 (counts = %v)", n, got)
	}
	days, err := counter.Read(context.Background(), "key:kid_t", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 0 {
		t.Errorf("legacy total advanced on a 429: %+v — throttled requests must not eat quota", days)
	}
}

// TestUsageTracker_ReadDeadlineMark — a 5xx answered after a server-side
// read deadline fired is billable (the read spent its budget); the mark never
// makes a 429 billable, and outside the tracker it is a harmless no-op.
func TestUsageTracker_ReadDeadlineMark(t *testing.T) {
	middleware.MarkReadDeadline(context.Background())

	for _, tc := range []struct {
		status int
		want   int64
	}{
		{http.StatusServiceUnavailable, 1},
		{http.StatusInternalServerError, 1},
		{http.StatusTooManyRequests, 0},
	} {
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		counter := usage.New(rdb)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/price", func(w http.ResponseWriter, r *http.Request) {
			middleware.MarkReadDeadline(r.Context())
			w.WriteHeader(tc.status)
		})
		stamp := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_rd"}
				next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), s)))
			})
		}
		h := middleware.Chain(mux, stamp, middleware.UsageTracker(counter, nil))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/price", nil))
		if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
			t.Fatal("after-response pool did not drain in time")
		}

		got, err := counter.MonthToDate(context.Background(), "key:kid_rd")
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("status %d with deadline mark: billable = %d, want %d", tc.status, got, tc.want)
		}
		if n := detailCounts(t, counter, "key:kid_rd"); len(n) != 1 {
			t.Errorf("status %d: detail rows = %v, want exactly one", tc.status, n)
		}
		_ = rdb.Close()
	}
}

// TestUsageTracker_UnmatchedRouteBuckets — a 404 on an unregistered
// path buckets under the bounded "unmatched" family, never the raw
// path.
func TestUsageTracker_UnmatchedRouteBuckets(t *testing.T) {
	ts, counter := usageTestStack(t, auth.Subject{
		Tier:  auth.TierAPIKey,
		KeyID: "kid_u",
	}, "GET /v1/registered", http.StatusOK)

	resp, err := http.Get(ts.URL + "/v1/definitely-not-a-route/xyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}

	got := detailCounts(t, counter, "key:kid_u")
	if n := got[[2]string{"unmatched", usage.ClassClientError}]; n != 1 {
		t.Errorf("unmatched 4xx count = %d, want 1 (counts = %v)", n, got)
	}
}

// TestUsageTracker_ResolveRoute_ThrottledBeforeDispatch — a 429 from a
// gate that rejects BEFORE the mux ever dispatches (RateLimit, in
// production) must still bucket under the real route pattern, not the
// bounded "unmatched" fallback. Without middleware.ResolveRoute wired
// ahead of the gate, obs.CaptureRoute never runs (the mux is never
// reached) and endpointFamily() had no route information left — every
// production 429 landed under "unmatched" instead of its real
// endpoint.
func TestUsageTracker_ResolveRoute_ThrottledBeforeDispatch(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/price", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_gate"}
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(auth.WithSubject(r.Context(), subject))
			next.ServeHTTP(w, r)
		})
	}
	// denyBeforeDispatch mimics RateLimit: it 429s WITHOUT calling
	// next, so the mux (and obs.CaptureRoute) never run — exactly the
	// production ordering server.go's Handler() wires.
	denyBeforeDispatch := func(_ http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		})
	}

	h := middleware.Chain(mux, stamp, middleware.ResolveRoute(mux),
		middleware.UsageTracker(counter, nil), denyBeforeDispatch)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v1/price")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}

	got := detailCounts(t, counter, "key:kid_gate")
	if n := got[[2]string{"/v1/price", usage.ClassThrottled}]; n != 1 {
		t.Errorf("throttled count for /v1/price = %d, want 1 (counts = %v)", n, got)
	}
	if n := got[[2]string{"unmatched", usage.ClassThrottled}]; n != 0 {
		t.Errorf("a gate-rejected request on a real route bucketed under 'unmatched': counts = %v", got)
	}
}

// TestUsageTracker_AnonymousSkipped — no subject → no counters at
// all (nothing to bill).
// A write-refusing Redis must not fail the request, but every unit it
// drops must be counted: the billable total is the monthly-quota input.
func TestUsageTracker_RefusedWriteCountsDroppedUnits(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	mr.SetError("MISCONF Redis is configured to save RDB snapshots, but it's currently unable to persist to disk")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/price", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_drop"}
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), sub)))
		})
	}
	h := middleware.Chain(mux, stamp, middleware.UsageTracker(usage.New(rdb), nil))

	billable := obs.UsageUnitsDroppedTotal.WithLabelValues(obs.UsageCounterBillable)
	detail := obs.UsageUnitsDroppedTotal.WithLabelValues(obs.UsageCounterDetail)
	b0, d0 := testutil.ToFloat64(billable), testutil.ToFloat64(detail)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price", nil))
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — metering must stay best-effort", w.Code)
	}
	if got := testutil.ToFloat64(billable) - b0; got != 1 {
		t.Errorf("billable units dropped delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(detail) - d0; got != 1 {
		t.Errorf("detail units dropped delta = %v, want 1", got)
	}
}

func TestUsageTracker_AnonymousSkipped(t *testing.T) {
	ts, counter := usageTestStack(t, auth.Subject{}, "GET /v1/price", http.StatusOK)
	resp, err := http.Get(ts.URL + "/v1/price")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rows, err := counter.ScanDetail(context.Background(), lastTwoUTCDates())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("anonymous request produced detail rows: %+v", rows)
	}
}

// TestUsageTracker_PanickingHandlerStillCounted. Recoverer
// sits OUTSIDE UsageTracker in the real stack (server.go), so a
// panicking handler unwinds past the tracker's post-dispatch
// bookkeeping. Straight-line code after next.ServeHTTP never runs on
// that unwind; the fix defers it so it does.
func TestUsageTracker_PanickingHandlerStillCounted(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/price", func(w http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_panic"}
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), sub)))
		})
	}
	// Recoverer OUTSIDE UsageTracker, matching server.go's stack order.
	h := middleware.Chain(mux, stamp, middleware.Recoverer(nil), middleware.UsageTracker(counter, nil))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price", nil))
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (Recoverer)", w.Code)
	}

	subjectKey := middleware.UsageKeyForSubject(auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_panic"})
	got := detailCounts(t, counter, subjectKey)
	if got[[2]string{"/v1/price", usage.ClassServerError}] != 1 {
		t.Errorf("detail counts = %+v, want one 5xx row for /v1/price — a panicking request must still be counted", got)
	}
}

// TestUsageTracker_RecordsAfterClientAbort pins the post-response half of
// client-abort handling.
//
// UsageTracker's counters run AFTER the response is flushed, but they used
// `r.Context()` — which is already cancelled when the client aborted, or
// when the handler consumed the whole RequestTimeout budget. go-redis
// honours context cancellation, so the write failed and was swallowed at
// Debug level. The legacy total is the MONTHLY-QUOTA INPUT, so a lost row
// is lost billing signal, and moving RequestTimeout outward (the other half
// of the post-response change) would have widened the window.
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
	if total != 1 {
		t.Errorf("legacy usage total = %d, want 1 — a request whose context was cancelled "+
			"after the response was served must still be counted; the counter is the "+
			"monthly-quota input, so dropping it is lost billing signal AND a quota-evasion "+
			"vector (abort before the body completes)", total)
	}
}

// TestUsageTracker_AbortedRequestConsumesQuota pins the DECISION that came
// with the post-response change, so it cannot be silently reverted as
// "an unintended side effect".
//
// Without it an aborted request's Increment died on the cancelled
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
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}

	mtd, err := counter.MonthToDate(context.Background(), middleware.UsageKeyForSubject(subject))
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if mtd != attempts {
		t.Errorf("month-to-date = %d after %d aborted requests, want %d — aborting before "+
			"the body completes must NOT buy unmetered traffic", mtd, attempts, attempts)
	}
}

// TestUsageTracker_DoesNotBlockRequestGoroutineOnWedgedStore is the core
// regression: UsageTracker must not run its counter writes INLINE
// on the request goroutine, under context.WithoutCancel(r.Context()) +
// postResponseWriteTimeout, entirely OUTSIDE api.request_timeout. A slow
// store (here, a Redis with every command delayed 300ms) added a
// multiple of that delay directly to the request's wall-clock latency —
// two writes (legacy total + detail) meant ~600ms tacked onto a request
// net/http had already buffered and could have flushed in microseconds.
//
// The fix hands both writes to the shared after-response pool
// (middleware.AfterResponse), so ServeHTTP returns as soon as the
// handler and the flush are done, regardless of how slow the store is.
func TestUsageTracker_DoesNotBlockRequestGoroutineOnWedgedStore(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	rdb.AddHook(slowRedisHook{delay: 300 * time.Millisecond})
	counter := usage.New(rdb)

	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_slow", Identifier: "acct:slow"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/price", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}
	h := middleware.Chain(mux, stamp, middleware.UsageTracker(counter, nil))

	start := time.Now()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price", nil))
	elapsed := time.Since(start)

	const budget = 100 * time.Millisecond // well under one 300ms store round trip
	if elapsed > budget {
		t.Fatalf("ServeHTTP took %v against a store that delays every command 300ms — "+
			"UsageTracker must not run its post-response counter writes on the request "+
			"goroutine (GH-627)", elapsed)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	if !middleware.AfterResponseDrainForTest(2 * time.Second) {
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
	if total != 1 {
		t.Errorf("legacy usage total = %d, want 1 — the deferred write must still land "+
			"once the pool catches up", total)
	}
}

// TestUsageTracker_StreamCountedAtOpen pins that an SSE stream is
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
// not eat monthly quota, while the detail family still sees it.
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

// TestUsageTracker_StreamTickCountBounded pins the bound on the per-tick count:
// counting an SSE stream once at open (TestUsageTracker_StreamCountedAtOpen)
// still lets it buy unbounded duration for that one unit. A stream held
// open for a known duration must accrue additional billable units for
// each meter interval it survives (never starved) but not more than
// that plus scheduling slack (never double-billed).
func TestUsageTracker_StreamTickCountBounded(t *testing.T) {
	const interval = 20 * time.Millisecond
	t.Cleanup(middleware.SetStreamMeterIntervalForTest(interval))

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)
	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_stream_tick", Identifier: "acct:stream_tick"}
	id := middleware.UsageKeyForSubject(subject)

	_, wait := runOpenStream(t, counter, subject, http.StatusOK)

	const wantTicks = 5
	time.Sleep(wantTicks * interval)
	wait()

	// Ticks write synchronously on their own goroutine (see
	// meterOpenStream), so MonthToDate is read directly once the
	// stream has closed rather than through streamUsageTotal's
	// after-response drain.
	got, err := counter.MonthToDate(context.Background(), id)
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	const (
		wantMin = int64(1) // the open bill alone, if ticks never fired
		slack   = int64(2) // scheduling jitter, either direction
	)
	wantMax := int64(1+wantTicks) + slack
	if got < wantMin || got > wantMax {
		t.Fatalf("usage total for a stream held open ~%d meter intervals = %d, want in [%d, %d] "+
			"(below min: periodic re-metering never advanced it; above max: a tick double-billed)",
			wantTicks, got, wantMin, wantMax)
	}
}

// TestUsageTracker_StreamEndedAtMonthlyQuota: MonthlyQuota admits a stream
// only while the account is under its cap, so a stream opened just under it
// must be ended by the tick that finds the cap reached, never billed past it.
func TestUsageTracker_StreamEndedAtMonthlyQuota(t *testing.T) {
	const interval = 20 * time.Millisecond
	t.Cleanup(middleware.SetStreamMeterIntervalForTest(interval))

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)
	const quota = 3
	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_stream_quota", Identifier: "acct:stream_quota", MonthlyQuota: quota}
	id := middleware.UsageKeyForSubject(subject)

	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/price/tip/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		select {
		case <-r.Context().Done():
		case <-unblock:
		}
	})
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}
	h := middleware.Chain(mux, stamp, middleware.UsageTracker(counter, nil))
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/price/tip/stream", nil))
	}()

	select {
	case <-ended:
	case <-time.After(100 * interval):
		got, _ := counter.MonthToDate(context.Background(), id)
		t.Fatalf("stream still open after ~100 meter intervals with usage %d against quota %d — "+
			"a tick at the monthly cap must end the stream", got, quota)
	}
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}
	got, err := counter.MonthToDate(context.Background(), id)
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if got != quota {
		t.Fatalf("usage total for a stream ended at its monthly quota = %d, want exactly %d "+
			"(below: ended before the cap; above: a tick billed past it)", got, quota)
	}
}

// TestUsageTracker_StreamNoTicksAfterClose guards meterOpenStream's
// shutdown: once a stream's handler has returned, no further tick may
// land, however many meter intervals later something looks again.
//
// Verified adversarially (1µs interval, 3000 closes) before the
// defer-reorder + non-blocking done re-check in meterOpenStream: the
// ticker's own shutdown could still race a simultaneously-ready tick
// and leak ~1 extra billable unit per 3000 closes. That residual
// select-fairness tie is not fully eliminable short of a mutex
// serializing every tick against close (disproportionate given it
// requires a tick to land within nanoseconds of close even at a 1µs
// interval, and is irrelevant at the real 1-minute production
// interval); this test uses a realistic interval, at which it does
// not flake.
func TestUsageTracker_StreamNoTicksAfterClose(t *testing.T) {
	const interval = 5 * time.Millisecond
	t.Cleanup(middleware.SetStreamMeterIntervalForTest(interval))

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)
	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_stream_close", Identifier: "acct:stream_close"}
	id := middleware.UsageKeyForSubject(subject)

	_, wait := runOpenStream(t, counter, subject, http.StatusOK)

	// Let at least one periodic tick land before closing, so this
	// test exercises meterOpenStream's loop, not just the open bill.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := counter.MonthToDate(context.Background(), id)
		if err != nil {
			t.Fatalf("MonthToDate: %v", err)
		}
		if got >= 2 {
			break
		}
		time.Sleep(interval)
	}
	wait()

	closedTotal, err := counter.MonthToDate(context.Background(), id)
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}

	// Several more meter intervals, well after the stream closed and
	// with no request in flight.
	time.Sleep(10 * interval)

	after, err := counter.MonthToDate(context.Background(), id)
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if after != closedTotal {
		t.Fatalf("usage total grew from %d to %d after the stream's handler returned — "+
			"a tick fired after close", closedTotal, after)
	}
}

// TestUsageTracker_StreamPanicBeforeOpenBillsNothing pins the
// defer-ordering hazard fixed alongside the periodic re-meter: a
// handler that panics before ever writing anything never opens the
// stream (once.fired() stays false the whole time it runs), so
// UsageTracker's recover defer is the FIRST call to once.fire —
// classing the request as a platform 5xx, which must never be billed.
//
// If that recover defer runs BEFORE the streaming block's
// close(done) (defers are LIFO, so close(done), registered first, runs
// second), meterOpenStream's ticker keeps running for a
// window after once.fire has already flipped fired()==true. A tick
// landing in that window reads rec's still-default 200 status (a panic
// never sets it) and bills it as OK — eating quota on a request this
// same defer had just classed as non-billable. A 1µs meter interval
// plus many trials makes the (otherwise sub-microsecond) window
// observable: without the guard this leaks on about 113 of 3000 trials
// in development.
func TestUsageTracker_StreamPanicBeforeOpenBillsNothing(t *testing.T) {
	t.Cleanup(middleware.SetStreamMeterIntervalForTest(time.Microsecond))

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)
	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_stream_panic", Identifier: "acct:stream_panic"}
	id := middleware.UsageKeyForSubject(subject)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/price/tip/stream", func(http.ResponseWriter, *http.Request) {
		panic("boom before open")
	})
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}
	// Recoverer OUTSIDE UsageTracker, matching server.go's stack order.
	h := middleware.Chain(mux, stamp, middleware.Recoverer(nil), middleware.UsageTracker(counter, nil))

	const trials = 500
	for i := 0; i < trials; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price/tip/stream", nil))
	}
	// Give any still-running per-request ticker goroutine a moment to
	// (mis)fire before reading — at a 1µs interval they exit almost
	// immediately once done closes, but "almost" is exactly the gap
	// under test.
	time.Sleep(50 * time.Millisecond)
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}

	got, err := counter.MonthToDate(context.Background(), id)
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if got != 0 {
		t.Fatalf("billable usage total after %d panic-before-open streams = %d, want 0 "+
			"(a tick landed between once.fire and close(done) and billed rec's still-default "+
			"200 status — COR-05 violation)", trials, got)
	}
}

// TestUsageTracker_StreamPanicDuringTickBillsNothing pins the race left
// after the defer-reorder fix: close(done) only SIGNALS
// meterOpenStream to stop, it does not wait for it to have stopped. A
// tick that already passed its done re-check when the handler panics
// can be paused right there while close(done) then once.fire(true) both
// run, then resume and read once.fired()==true — billing rec's
// still-default 200 status as OK on a request the recover defer just
// classed as a non-billable 5xx, and double-billing besides.
//
// A test hook (SetStreamTickHookForTest) pauses the goroutine at
// exactly that point instead of relying on scheduling luck to land
// there, making the race deterministic rather than probabilistic. Once
// paused, the handler is let panic and its defers run as far as they
// structurally can: close(done), then block on <-exited. once.fire is
// sequenced strictly after <-exited unblocks in the same goroutine, so
// it CANNOT have run while the hook holds this goroutine — no matter
// how long the test waits — only with the exited wait in place. Fails
// without the wait (close(done) alone); passes with it.
func TestUsageTracker_StreamPanicDuringTickBillsNothing(t *testing.T) {
	t.Cleanup(middleware.SetStreamMeterIntervalForTest(5 * time.Millisecond))

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)
	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_stream_tick_panic", Identifier: "acct:stream_tick_panic"}
	id := middleware.UsageKeyForSubject(subject)

	hookReached := make(chan struct{})
	releaseHook := make(chan struct{})
	var hookOnce sync.Once
	t.Cleanup(middleware.SetStreamTickHookForTest(func() {
		hookOnce.Do(func() { close(hookReached) })
		<-releaseHook
	}))

	allowPanic := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/price/tip/stream", func(http.ResponseWriter, *http.Request) {
		<-allowPanic
		panic("boom mid-tick")
	})
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}
	// Recoverer OUTSIDE UsageTracker, matching server.go's stack order.
	h := middleware.Chain(mux, stamp, middleware.Recoverer(nil), middleware.UsageTracker(counter, nil))

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/price/tip/stream", nil))
	}()

	select {
	case <-hookReached:
	case <-time.After(5 * time.Second):
		t.Fatal("stream tick never reached the test hook")
	}

	// Let the handler panic. Its defers can now run only as far as
	// close(done) and then block on <-exited, since meterOpenStream is
	// paused inside the hook. once.fire cannot have run yet — that is
	// structural, not timing — so this sleep is just giving the
	// scheduler room to get there, not a wait for a race to resolve.
	close(allowPanic)
	time.Sleep(50 * time.Millisecond)

	close(releaseHook)

	select {
	case <-reqDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stream handler never returned")
	}

	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}

	// A handler that does not wait for meterOpenStream at all makes
	// reqDone closing prove nothing about whether the released tick's
	// own IncrementBy call has landed yet — poll rather than assume it.
	// This does not reintroduce timing-dependence in the race itself
	// (that part is already forced deterministically by the hook above):
	// it only tolerates ordinary write latency before asserting absence.
	var got int64
	var err error
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		got, err = counter.MonthToDate(context.Background(), id)
		if err != nil {
			t.Fatalf("MonthToDate: %v", err)
		}
		if got != 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got != 0 {
		t.Fatalf("billable usage total after a panic mid-tick = %d, want 0 (the paused tick "+
			"resumed, read once.fired()==true, and billed rec's still-default 200 status as "+
			"OK — COR-05 violation)", got)
	}
}

// TestUsageTracker_CountsUnderOwnerAccountNotCredential — the writer
// must record under the owner account, so traffic from two different
// credentials of the same account lands on ONE month-to-date counter
// and a revoke-and-mint cannot hand the caller a fresh one.
func TestUsageTracker_CountsUnderOwnerAccountNotCredential(t *testing.T) {
	ts, counter, setSubject := accountScopeStack(t)
	ctx := context.Background()

	setSubject(apiKeySubject("acme", "kid_old", 0))
	getPrice(t, ts)
	getPrice(t, ts)
	// Revoke-and-mint: same account, brand-new credential.
	setSubject(apiKeySubject("acme", "kid_new", 0))
	getPrice(t, ts)

	accountKey := "id:" + auth.AccountIdentifier("acme")
	got, err := counter.MonthToDate(ctx, accountKey)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Errorf("month-to-date under %q = %d, want 3 (all three requests belong to one account's plan budget)", accountKey, got)
	}
	for _, credKey := range []string{"key:kid_old", "key:kid_new"} {
		n, err := counter.MonthToDate(ctx, credKey)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("month-to-date under %q = %d, want 0 — a per-credential counter splits the account's allowance", credKey, n)
		}
	}
}
