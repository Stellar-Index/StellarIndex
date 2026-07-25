package v1_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// C3-015 (audit-2026-07-23) — self-service key-mint quota.
//
// POST /v1/account/keys minted on every call with no count check, so
// one authenticated caller could mint live credentials in a loop until
// the store filled. These pin the ceiling, its wire shape, and the two
// boundaries (one under, disabled).

// newAccountQuotaTestServer wires the self-service surface with an
// explicit per-identifier key quota.
func newAccountQuotaTestServer(t *testing.T, subject auth.Subject, store v1.AccountStore, quota int) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{
		Auth:            fakeAuthMiddleware(subject),
		Accounts:        store,
		AccountKeyQuota: quota,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// seedKeys builds n active key records for identifier.
func seedKeys(identifier string, n int) []auth.APIKeyRecord {
	out := make([]auth.APIKeyRecord, 0, n)
	for i := range n {
		out = append(out, auth.APIKeyRecord{
			KeyID:      fmt.Sprintf("kid_%s_%d", identifier, i),
			Identifier: identifier,
			Tier:       auth.TierAPIKey,
		})
	}
	return out
}

// TestAccountKeysCreate_QuotaExceeded is the core regression: a caller
// already at the ceiling is refused with 409 and the store's Create is
// never reached.
func TestAccountKeysCreate_QuotaExceeded(t *testing.T) {
	store := &fakeAccountStore{
		listed: map[string][]auth.APIKeyRecord{"owner-42": seedKeys("owner-42", 3)},
	}
	ts := newAccountQuotaTestServer(t, auth.Subject{
		Identifier: "owner-42",
		Tier:       auth.TierAPIKey,
	}, store, 3)

	resp, err := http.Post(ts.URL+"/v1/account/keys", "application/json",
		strings.NewReader(`{"label":"one-too-many"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (quota reached)", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("Create was called %d times; a refused mint must never issue a credential", store.calls)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var problem struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Type != "https://api.stellarindex.io/errors/key-quota-exceeded" {
		t.Errorf("problem type = %q, want .../errors/key-quota-exceeded", problem.Type)
	}
	if problem.Status != http.StatusConflict {
		t.Errorf("problem status = %d, want 409", problem.Status)
	}
	if !strings.Contains(problem.Detail, "3 active API keys") || !strings.Contains(problem.Detail, "max 3") {
		t.Errorf("problem detail = %q, want the current count + ceiling so the caller knows what to revoke", problem.Detail)
	}
}

// TestAccountKeysCreate_UnderQuotaMints pins the boundary below the
// ceiling: the mint still happens, and revoked records do not consume
// quota.
func TestAccountKeysCreate_UnderQuotaMints(t *testing.T) {
	keys := seedKeys("owner-42", 3)
	keys[0].RevokedAt = keys[0].CreatedAt.AddDate(0, 0, 1) // revoked — must not count
	store := &fakeAccountStore{
		listed: map[string][]auth.APIKeyRecord{"owner-42": keys},
		rec:    auth.APIKeyRecord{KeyID: "kid_new", Label: "fresh"},
		plain:  "sip_freshplaintext",
	}
	ts := newAccountQuotaTestServer(t, auth.Subject{
		Identifier: "owner-42",
		Tier:       auth.TierAPIKey,
	}, store, 3)

	resp, err := http.Post(ts.URL+"/v1/account/keys", "application/json",
		strings.NewReader(`{"label":"fresh"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (2 active < quota 3)", resp.StatusCode)
	}
	if store.calls != 1 {
		t.Errorf("Create calls = %d, want 1", store.calls)
	}
}

// TestAccountKeysCreate_QuotaDefaultApplies pins that an unset
// Options.AccountKeyQuota still enforces a ceiling — the whole defect
// was an unbounded default.
func TestAccountKeysCreate_QuotaDefaultApplies(t *testing.T) {
	store := &fakeAccountStore{
		listed: map[string][]auth.APIKeyRecord{"owner-42": seedKeys("owner-42", 25)},
	}
	ts := newAccountTestServer(t, auth.Subject{
		Identifier: "owner-42",
		Tier:       auth.TierAPIKey,
	}, store)

	resp, err := http.Post(ts.URL+"/v1/account/keys", "application/json",
		strings.NewReader(`{"label":"twenty-sixth"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (default quota of 25 reached)", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("Create calls = %d, want 0", store.calls)
	}
}

// TestAccountKeysCreate_QuotaDisabled pins the operator escape hatch: a
// negative quota turns the check off entirely (and skips the count read).
func TestAccountKeysCreate_QuotaDisabled(t *testing.T) {
	store := &fakeAccountStore{
		listed: map[string][]auth.APIKeyRecord{"owner-42": seedKeys("owner-42", 500)},
		rec:    auth.APIKeyRecord{KeyID: "kid_new"},
		plain:  "sip_x",
	}
	ts := newAccountQuotaTestServer(t, auth.Subject{
		Identifier: "owner-42",
		Tier:       auth.TierAPIKey,
	}, store, -1)

	resp, err := http.Post(ts.URL+"/v1/account/keys", "application/json",
		strings.NewReader(`{"label":"unbounded-by-config"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (quota disabled)", resp.StatusCode)
	}
	if store.listCalls != 0 {
		t.Errorf("list calls = %d, want 0 (disabled check must not read the store)", store.listCalls)
	}
}

// TestAccountKeysCreate_QuotaReadFailureFailsClosed pins the direction
// of the degrade: if the count can't be read, the mint is refused. The
// check exists because an unbounded mint is the abuse, so "couldn't
// count, mint anyway" would hand the abuser the bypass.
func TestAccountKeysCreate_QuotaReadFailureFailsClosed(t *testing.T) {
	store := &fakeAccountStore{listErr: errors.New("redis blip")}
	ts := newAccountQuotaTestServer(t, auth.Subject{
		Identifier: "owner-42",
		Tier:       auth.TierAPIKey,
	}, store, 3)

	resp, err := http.Post(ts.URL+"/v1/account/keys", "application/json",
		strings.NewReader(`{"label":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (fail closed)", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("Create calls = %d, want 0 — a mint must not proceed on an unverifiable quota", store.calls)
	}
}
