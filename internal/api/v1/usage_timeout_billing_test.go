package v1

import (
	"context"
	"errors"
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

// meteredBillable serves one request to handler through the production
// UsageTracker and returns the month-to-date BILLABLE count.
func meteredBillable(t *testing.T, handler http.HandlerFunc) (int, int64) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/markets", handler)
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_timeout"}
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}
	srv := middleware.Chain(mux, stamp, middleware.UsageTracker(counter, nil))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/markets", nil))
	// UsageTracker's counter write runs on the shared after-response pool,
	// not inline, so a read right after ServeHTTP returns must
	// wait for it to land first.
	if !middleware.AfterResponseDrainForTest(2 * time.Second) {
		t.Fatal("after-response pool did not drain in time")
	}

	billable, err := counter.MonthToDate(context.Background(), "key:kid_timeout")
	if err != nil {
		t.Fatal(err)
	}
	return w.Code, billable
}

// A server-side read deadline is answered 503, and a 5xx is otherwise
// unbillable — so without the deadline mark the most expensive
// request shape metered at zero while a 2 ms 404 metered at one.
func TestReadDeadline503_DebitsMonthlyQuota(t *testing.T) {
	// A driver that re-phrases the cancellation (pg SQLSTATE 57014).
	pgCancel := errors.New("ERROR: canceling statement due to user request (SQLSTATE 57014)")

	cases := []struct {
		name         string
		handler      http.HandlerFunc
		wantBillable int64
	}{
		{"per-call budget via handlerTimedOut", func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 0)
			defer cancel()
			<-ctx.Done()
			if handlerTimedOut(ctx, pgCancel) {
				writeProblem(w, r, "https://api.stellarindex.io/errors/markets-timeout",
					"Markets query timed out", http.StatusServiceUnavailable, "")
			}
		}, 1},
		{"blanket deadline via writeProblemErr", func(w http.ResponseWriter, r *http.Request) {
			writeProblemErr(w, r, context.DeadlineExceeded, "https://api.stellarindex.io/errors/internal",
				"Internal error", http.StatusInternalServerError, "")
		}, 1},
		{"fast-failing outage 503 stays unbillable", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, r, "https://api.stellarindex.io/errors/cache-unavailable",
				"Cache unavailable", http.StatusServiceUnavailable, "")
		}, 0},
		{"plain 500 stays unbillable", func(w http.ResponseWriter, r *http.Request) {
			writeProblemErr(w, r, errors.New("boom"), "https://api.stellarindex.io/errors/internal",
				"Internal error", http.StatusInternalServerError, "")
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, billable := meteredBillable(t, tc.handler)
			if code < 500 {
				t.Fatalf("status = %d, want a 5xx", code)
			}
			if billable != tc.wantBillable {
				t.Fatalf("month-to-date billable = %d, want %d", billable, tc.wantBillable)
			}
		})
	}
}

// TestUsageTracker_ObservesQuotaDenials pins that UsageTracker is not appended after MonthlyQuota: middleware.Chain
// makes an earlier entry the outer one and a quota denial skips next, so a quota 429 would never reach the tracker
// and a capped customer would show zero traffic. It must sit outside both 429 producers (quota and rate-limit).
func TestUsageTracker_ObservesQuotaDenials(t *testing.T) {
	var trackerRan bool
	tracker := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			trackerRan = true
			next.ServeHTTP(w, r)
		})
	}
	// Stand-in for the real MonthlyQuota middleware's deny path: write
	// the 429 and return WITHOUT calling next.
	quotaDeny := func(_ http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		})
	}

	srv := New(Options{UsageTracker: tracker, MonthlyQuota: quotaDeny})

	rec := httptest.NewRecorder()
	// An existing, always-mounted route — no new registration, so the
	// route↔OpenAPI inventory lint stays satisfied.
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (the quota stand-in must have denied)", rec.Code)
	}
	if !trackerRan {
		t.Error("the usage tracker did NOT run for a monthly-quota 429: quota denials are counted " +
			"nowhere, so a capped customer's usage report reads as zero traffic")
	}
}

// TestUsageTracker_ObservesRateLimitDenials — the same guarantee for
// the other 429 producer, which the pre-existing ordering did hold.
// Pinned so a future reorder can't fix one and break the other.
func TestUsageTracker_ObservesRateLimitDenials(t *testing.T) {
	var trackerRan bool
	tracker := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			trackerRan = true
			next.ServeHTTP(w, r)
		})
	}
	limitDeny := func(_ http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		})
	}

	srv := New(Options{UsageTracker: tracker, RateLimit: limitDeny})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if !trackerRan {
		t.Error("the usage tracker did NOT run for a rate-limit 429")
	}
}
