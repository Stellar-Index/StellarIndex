package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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
// total. wantBillable is 0 for 5xx: that used to be 1, which is
// exactly the COR-05 defect (a platform failure charged to the
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
// total (COR-05).
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
// endpoint (Q177).
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

// TestUsageTracker_PanickingHandlerStillCounted — GH-1276. Recoverer
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
