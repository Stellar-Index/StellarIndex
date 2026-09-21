package v1_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// fakeSessionPeeker is the handler-level test double for
// [v1.SessionPeeker]. Returns a canned SessionInfo regardless of
// context, mirroring how dashboardauth's real implementation is
// wired via main.go's adapter.
type fakeSessionPeeker struct {
	info v1.SessionInfo
	ok   bool
}

func (f *fakeSessionPeeker) SessionFromContext(_ context.Context) (v1.SessionInfo, bool) {
	return f.info, f.ok
}

// TestAccountUsage_SessionAuthenticated — RLT-415 / GH #796. A
// magic-link dashboard session with NO API key attached must read
// its account's usage, not 401. Pre-fix, handleAccountUsage gated
// solely on auth.SubjectFrom, which a session-only request never
// populates (only the API-key auth middleware calls auth.WithSubject
// in production) — every signed-in dashboard user got a 401 the
// frontend silently swallowed into an empty usage page.
func TestAccountUsage_SessionAuthenticated(t *testing.T) {
	rollup := &fakeUsageRollupReader{rows: []v1.UsageEndpointDay{
		{Date: "2026-07-02", Endpoint: "/v1/price", Requests: 10, Errors: 0, Throttled: 0},
	}}
	srv := v1.New(v1.Options{
		// Anonymous — the request carries a session cookie, not an
		// API key, so no auth.Subject reaches the context.
		Auth:              fakeAuthMiddleware(auth.Subject{}),
		SessionPeeker:     &fakeSessionPeeker{ok: true, info: v1.SessionInfo{AccountSlug: "acme-labs"}},
		UsageRollupReader: rollup,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v1/account/usage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a dashboard session must not 401 on /v1/account/usage)", resp.StatusCode)
	}
	// Must read under the SAME account-scoped key an API key minted
	// on this account would write under (middleware.UsageKeyForSubject's
	// Identifier branch), so the dashboard sees the account's real
	// usage rather than an unrelated / empty bucket.
	if rollup.gotSubject != "id:acct:acme-labs" {
		t.Errorf("subject = %q, want id:acct:acme-labs", rollup.gotSubject)
	}
}

// TestAccountUsage_NoSessionNoSubject_Unauthenticated — a request
// with neither a session nor an API key still 401s: the session path
// must not become a universal bypass.
func TestAccountUsage_NoSessionNoSubject_Unauthenticated(t *testing.T) {
	srv := v1.New(v1.Options{
		Auth:          fakeAuthMiddleware(auth.Subject{}),
		SessionPeeker: &fakeSessionPeeker{ok: false},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v1/account/usage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
