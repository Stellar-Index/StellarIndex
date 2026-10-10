// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// drainAfterResponse blocks until the shared after-response pool has
// finished every task submitted so far. UsageTracker's counter writes
// run there, so a test that immediately fires the NEXT request
// — expecting MonthlyQuota to observe the PREVIOUS one's increment —
// must synchronize on this first; otherwise it is racing the async
// write against the following request's read.
func drainAfterResponse(t *testing.T) {
	t.Helper()
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}
}

// The monthly ceiling is a PLAN budget, so the counter it is
// enforced against must be keyed on the owner ACCOUNT. Keying it on the
// credential let a customer multiply the plan allowance by the number
// of keys held and reset it mid-month by revoking and re-minting.
//
// These two tests run the real writer (UsageTracker) and the real
// reader (MonthlyQuota, over a real usage.Counter on miniredis) as one
// loop, because the defect only exists in the pairing: a reader pointed
// at an account key the writer never writes meters nothing at all.

// accountScopeStack wires stamp → UsageTracker → MonthlyQuota → handler
// over one miniredis-backed counter. The subject is swappable so a test
// can rotate the caller's credential mid-run (revoke-and-mint) while
// keeping the owner account fixed. UsageTracker sits OUTSIDE the quota
// gate, as in production, so a quota 429 is observed and classed as
// throttled rather than billed.
func accountScopeStack(t *testing.T) (*httptest.Server, *usage.Counter, func(auth.Subject)) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)

	var (
		mu      sync.Mutex
		subject auth.Subject
	)
	setSubject := func(s auth.Subject) {
		mu.Lock()
		defer mu.Unlock()
		subject = s
	}
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			s := subject
			mu.Unlock()
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), s)))
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/price", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := middleware.Chain(mux, stamp,
		middleware.UsageTracker(counter, nil),
		middleware.MonthlyQuota(counter, nil),
	)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, counter, setSubject
}

// apiKeySubject is a Postgres-validator-shaped API-key subject: an
// owner-account Identifier (auth.AccountIdentifier) plus the public
// KeyID of the credential presented.
func apiKeySubject(slug, keyID string, quota int64) auth.Subject {
	return auth.Subject{
		Identifier:   auth.AccountIdentifier(slug),
		KeyID:        keyID,
		Tier:         auth.TierAPIKey,
		MonthlyQuota: quota,
	}
}

// getPrice issues one request and drains the after-response pool before
// returning, so callers issuing several requests in sequence can rely on
// each one's usage-counter write having landed before the next fires —
// exactly the ordering MonthlyQuota's enforcement depends on.
func getPrice(t *testing.T, ts *httptest.Server) *http.Response {
	t.Helper()
	resp, err := http.Get(ts.URL + "/v1/price")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	drainAfterResponse(t)
	return resp
}
