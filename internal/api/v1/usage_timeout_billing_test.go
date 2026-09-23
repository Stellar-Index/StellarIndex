package v1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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

	billable, err := counter.MonthToDate(context.Background(), "key:kid_timeout")
	if err != nil {
		t.Fatal(err)
	}
	return w.Code, billable
}

// A server-side read deadline is answered 503, and a 5xx is otherwise
// unbillable (COR-05) — so without the deadline mark the most expensive
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
