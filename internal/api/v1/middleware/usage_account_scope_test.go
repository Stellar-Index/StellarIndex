// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware_test

import (
	"context"
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

// RLT-404 — the monthly ceiling is a PLAN budget, so the counter it is
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

func getPrice(t *testing.T, ts *httptest.Server) *http.Response {
	t.Helper()
	resp, err := http.Get(ts.URL + "/v1/price")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
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

// TestMonthlyQuota_CapSurvivesKeyRotationAndDoesNotMultiply — the
// enforcement half of the same loop. A cap of 2 is spent by one key;
// a freshly minted key on the SAME account must inherit the exhausted
// counter (429), while an unrelated account is untouched (200).
func TestMonthlyQuota_CapSurvivesKeyRotationAndDoesNotMultiply(t *testing.T) {
	ts, _, setSubject := accountScopeStack(t)

	const cap2 = 2
	setSubject(apiKeySubject("acme", "kid_old", cap2))
	for i := 1; i <= cap2; i++ {
		if resp := getPrice(t, ts); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d under cap: status = %d, want 200", i, resp.StatusCode)
		}
	}
	if resp := getPrice(t, ts); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("request at cap on the original key: status = %d, want 429", resp.StatusCode)
	}

	// Revoke-and-mint on the same account: the plan budget is spent,
	// so the new credential must be denied too.
	setSubject(apiKeySubject("acme", "kid_new", cap2))
	resp := getPrice(t, ts)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("rotated key: status = %d, want 429 — a new KeyID must not reset the account's monthly counter", resp.StatusCode)
	}
	if used := resp.Header.Get("X-StellarIndex-Monthly-Used"); used != "2" {
		t.Errorf("rotated key: X-StellarIndex-Monthly-Used = %q, want \"2\" (the account's spend, not the credential's)", used)
	}
	if quota := resp.Header.Get("X-StellarIndex-Monthly-Quota"); quota != "2" {
		t.Errorf("rotated key: X-StellarIndex-Monthly-Quota = %q, want \"2\"", quota)
	}

	// A different account must NOT be caught by the same counter —
	// the fix scopes per account, it does not collapse all callers.
	setSubject(apiKeySubject("other-co", "kid_other", cap2))
	if other := getPrice(t, ts); other.StatusCode != http.StatusOK {
		t.Errorf("unrelated account: status = %d, want 200 — accounts must not share a counter", other.StatusCode)
	}
}
