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

// drainAfterResponse blocks until the after-response pool has finished every submitted task.
// UsageTracker's counter writes run there, so a test expecting MonthlyQuota to see the previous
// request's increment must drain first, or it races the async write.
func drainAfterResponse(t *testing.T) {
	t.Helper()
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}
}

// The monthly ceiling is a PLAN budget, so its counter must be keyed on the owner ACCOUNT, not
// the credential: otherwise a customer multiplies the allowance by the number of keys held, or
// resets it by revoking and re-minting.
//
// These tests run the real writer (UsageTracker) and reader (MonthlyQuota over a real
// usage.Counter on miniredis) as one loop, because the defect exists only in the pairing: a
// reader pointed at an account key the writer never writes meters nothing.

// accountScopeStack wires stamp, UsageTracker, MonthlyQuota and a handler over one miniredis
// counter. The subject is swappable to rotate the credential mid-run while the owner account
// stays fixed. UsageTracker sits OUTSIDE the quota gate, as in production, so a quota 429 is
// classed as throttled rather than billed.
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
