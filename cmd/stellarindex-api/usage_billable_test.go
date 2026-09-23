package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

type rollupRowSink struct{ rows []usage.RollupRow }

func (s *rollupRowSink) UpsertUsageDaily(_ context.Context, rows []usage.RollupRow) error {
	s.rows = append(s.rows, rows...)
	return nil
}

// TestUsageEndpointDay_BillableEqualsQuotaCounter pins GH-1278: the
// rollup path's `requests` includes 5xx while the MonthlyQuota counter
// excludes them, so no column of /v1/account/usage reconciled with a
// quota 429's month_to_date. Real traffic goes through UsageTracker,
// the real rollup folds it, and the billable column derived from the
// rollup must equal the counter the quota enforces.
func TestUsageEndpointDay_BillableEqualsQuotaCounter(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	counter := usage.New(rdb, usage.WithClock(func() time.Time { return now }))

	subject := auth.Subject{Identifier: auth.AccountIdentifier("acme"), Tier: auth.TierAPIKey}
	serve := middleware.UsageTracker(counter, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, _ := strconv.Atoi(r.URL.Query().Get("status"))
		w.WriteHeader(status)
	}))
	traffic := map[int]int{
		http.StatusOK:                  3,
		http.StatusNotFound:            2,
		http.StatusInternalServerError: 4,
		http.StatusTooManyRequests:     1,
	}
	for status, n := range traffic {
		for i := 0; i < n; i++ {
			req := httptest.NewRequest(http.MethodGet, "/v1/price?status="+strconv.Itoa(status), nil)
			req = req.WithContext(auth.WithSubject(req.Context(), subject))
			serve.ServeHTTP(httptest.NewRecorder(), req)
		}
	}

	sink := &rollupRowSink{}
	rollup := usage.NewRollup(counter, sink, time.Minute, nil)
	if _, err := rollup.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	var billable, requests int64
	for _, r := range sink.rows {
		day := usageEndpointDay(timescale.UsageDailyRow{
			Day: r.Day, Subject: r.Subject, Endpoint: r.Endpoint,
			OK: r.OK, ClientErrors: r.ClientErrors, ServerErrors: r.ServerErrors, Throttled: r.Throttled,
		})
		billable += day.Billable
		requests += day.Requests
	}

	quota, err := counter.MonthToDate(context.Background(), middleware.UsageKeyForSubject(subject))
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if quota != 5 {
		t.Fatalf("quota counter = %d, want 5 (3 ok + 2 4xx)", quota)
	}
	if billable != quota {
		t.Errorf("rollup billable = %d, quota counter = %d — must be equal", billable, quota)
	}
	if requests != 9 {
		t.Errorf("rollup requests = %d, want 9 (every non-429 response)", requests)
	}
}
