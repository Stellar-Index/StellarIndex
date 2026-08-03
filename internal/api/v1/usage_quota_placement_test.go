package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUsageTracker_ObservesQuotaDenials pins the cold-audit-2026-08-03
// finding: UsageTracker used to be appended AFTER MonthlyQuota, and
// middleware.Chain makes an earlier entry the OUTER one — so the quota
// middleware wrapped the usage tracker. A quota denial returns without
// calling next, which meant a monthly-quota 429 executed the tracker
// NOT AT ALL and was counted nowhere: a capped customer's usage report
// showed zero traffic rather than a wall of throttling, while the
// comments in internal/usage/counter.go and middleware/usage.go both
// claimed 429s "are still counted in full ... so neither becomes
// invisible".
//
// The tracker must sit OUTSIDE both 429 producers (quota and
// rate-limit) so it observes either denial.
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
