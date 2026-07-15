package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
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

// TestUsageTracker_OutcomeClasses — 4xx / 5xx land in their classes
// and still count in the legacy (allowed-traffic) total.
func TestUsageTracker_OutcomeClasses(t *testing.T) {
	cases := []struct {
		status int
		class  string
	}{
		{http.StatusNotFound, usage.ClassClientError},
		{http.StatusInternalServerError, usage.ClassServerError},
		{http.StatusNoContent, usage.ClassOK},
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

		got := detailCounts(t, counter, "key:kid_c")
		if n := got[[2]string{"/v1/thing", tc.class}]; n != 1 {
			t.Errorf("status %d: class %q count = %d, want 1 (counts = %v)",
				tc.status, tc.class, n, got)
		}
		days, err := counter.Read(context.Background(), "key:kid_c", 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(days) != 1 || days[0].Requests != 1 {
			t.Errorf("status %d: legacy total = %+v, want one day with 1 request", tc.status, days)
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

	got := detailCounts(t, counter, "key:kid_u")
	if n := got[[2]string{"unmatched", usage.ClassClientError}]; n != 1 {
		t.Errorf("unmatched 4xx count = %d, want 1 (counts = %v)", n, got)
	}
}

// TestUsageTracker_AnonymousSkipped — no subject → no counters at
// all (nothing to bill).
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
