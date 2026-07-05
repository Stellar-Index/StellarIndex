// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
	"github.com/StellarIndex/stellar-index/internal/auth"
	"github.com/StellarIndex/stellar-index/internal/platform"
)

// fakePlatformAccountStore implements v1.PlatformAccountStore over an
// in-memory map so the handler tests exercise the wire shape without
// standing up Postgres.
type fakePlatformAccountStore struct {
	byID        map[uuid.UUID]platform.Account
	getErr      error
	updateErr   error
	updateCalls int
	lastUpdate  platform.Account
}

func newFakePlatformAccountStore(a platform.Account) *fakePlatformAccountStore {
	return &fakePlatformAccountStore{byID: map[uuid.UUID]platform.Account{a.ID: a}}
}

func (f *fakePlatformAccountStore) Get(_ context.Context, id uuid.UUID) (platform.Account, error) {
	if f.getErr != nil {
		return platform.Account{}, f.getErr
	}
	a, ok := f.byID[id]
	if !ok {
		return platform.Account{}, platform.ErrNotFound
	}
	return a, nil
}

func (f *fakePlatformAccountStore) Update(_ context.Context, a platform.Account) error {
	f.updateCalls++
	f.lastUpdate = a
	if f.updateErr != nil {
		return f.updateErr
	}
	f.byID[a.ID] = a
	return nil
}

func newAdminAccountServer(t *testing.T, subject auth.Subject, store v1.PlatformAccountStore, sink v1.AuditSink) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{
		Auth:             fakeAuthMiddleware(subject),
		PlatformAccounts: store,
		Audit:            sink,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func patchJSON(t *testing.T, url, reason, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest PATCH %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if reason != "" {
		req.Header.Set("X-Reason", reason)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func seededAccount() platform.Account {
	return platform.Account{
		ID:     uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Name:   "Acme Corp",
		Slug:   "acme",
		Tier:   platform.TierStarter,
		Status: platform.AccountActive,
	}
}

// TestAdminAccountOverrides_Happy pins the operator tier-override path:
// the PATCH sets tier + both overrides, persists via Update, and lands
// one "account.override.set" audit row carrying the reason + before/after.
func TestAdminAccountOverrides_Happy(t *testing.T) {
	acct := seededAccount()
	store := newFakePlatformAccountStore(acct)
	sink := &recordingAuditSink{}
	ts := newAdminAccountServer(t, operatorSubject(), store, sink)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "enterprise comp",
		`{"tier":"enterprise","rate_limit_per_min_override":50000,"monthly_request_quota_override":200000000}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	if store.updateCalls != 1 {
		t.Fatalf("Update called %d times, want 1", store.updateCalls)
	}
	if store.lastUpdate.Tier != platform.TierEnterprise {
		t.Errorf("persisted Tier = %q, want enterprise", store.lastUpdate.Tier)
	}
	if store.lastUpdate.RateLimitPerMinOverride != 50000 {
		t.Errorf("persisted RateLimitPerMinOverride = %d, want 50000", store.lastUpdate.RateLimitPerMinOverride)
	}
	if store.lastUpdate.MonthlyRequestQuotaOverride != 200000000 {
		t.Errorf("persisted MonthlyRequestQuotaOverride = %d", store.lastUpdate.MonthlyRequestQuotaOverride)
	}

	var env struct {
		Data v1.AdminAccountView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Tier != "enterprise" || env.Data.RateLimitPerMinOverride != 50000 {
		t.Errorf("response view = %+v", env.Data)
	}

	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(sink.entries))
	}
	e := sink.entries[0]
	if e.Action != "account.override.set" || e.ActorKind != platform.ActorStaff ||
		e.TargetKind != "account" || e.TargetID != acct.ID.String() {
		t.Errorf("audit entry = %+v", e)
	}
	if !strings.Contains(string(e.Metadata), "enterprise comp") {
		t.Errorf("audit metadata missing reason: %s", e.Metadata)
	}
}

// TestAdminAccountOverrides_ClearOverride pins that override=0 clears
// (round-trips as inherit-tier-default: the store NULLIFs it).
func TestAdminAccountOverrides_ClearOverride(t *testing.T) {
	acct := seededAccount()
	acct.RateLimitPerMinOverride = 12345
	store := newFakePlatformAccountStore(acct)
	ts := newAdminAccountServer(t, operatorSubject(), store, nil)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "reset",
		`{"rate_limit_per_min_override":0}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if store.lastUpdate.RateLimitPerMinOverride != 0 {
		t.Errorf("override not cleared: %d", store.lastUpdate.RateLimitPerMinOverride)
	}
}

func TestAdminAccountOverrides_MissingReason400(t *testing.T) {
	acct := seededAccount()
	store := newFakePlatformAccountStore(acct)
	ts := newAdminAccountServer(t, operatorSubject(), store, nil)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "",
		`{"tier":"pro"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 without X-Reason", resp.StatusCode)
	}
	if store.updateCalls != 0 {
		t.Errorf("Update called despite missing reason")
	}
}

func TestAdminAccountOverrides_NonOperator403(t *testing.T) {
	acct := seededAccount()
	store := newFakePlatformAccountStore(acct)
	ts := newAdminAccountServer(t, auth.Subject{
		Identifier: "acct:customer", Tier: auth.TierAPIKey, KeyID: "kid_cust",
	}, store, nil)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "nope",
		`{"tier":"enterprise"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for non-operator", resp.StatusCode)
	}
	if store.updateCalls != 0 {
		t.Errorf("Update called by a non-operator")
	}
}

func TestAdminAccountOverrides_Anonymous401(t *testing.T) {
	acct := seededAccount()
	ts := newAdminAccountServer(t, auth.Subject{}, newFakePlatformAccountStore(acct), nil)
	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "x", `{"tier":"pro"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAdminAccountOverrides_NotFound404(t *testing.T) {
	acct := seededAccount()
	store := newFakePlatformAccountStore(acct)
	ts := newAdminAccountServer(t, operatorSubject(), store, nil)

	other := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+other.String(), "x", `{"tier":"pro"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for absent account", resp.StatusCode)
	}
}

func TestAdminAccountOverrides_Validation(t *testing.T) {
	acct := seededAccount()
	cases := []struct {
		name string
		body string
	}{
		{"empty patch", `{}`},
		{"bad tier", `{"tier":"platinum"}`},
		{"negative rate limit", `{"rate_limit_per_min_override":-5}`},
		{"rate limit too high", `{"rate_limit_per_min_override":200000}`},
		{"negative monthly quota", `{"monthly_request_quota_override":-1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakePlatformAccountStore(acct)
			ts := newAdminAccountServer(t, operatorSubject(), store, nil)
			resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "x", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			if store.updateCalls != 0 {
				t.Errorf("Update called on invalid input")
			}
		})
	}
}

func TestAdminAccountOverrides_StoreUnwired503(t *testing.T) {
	// No PlatformAccounts wired → 503.
	srv := v1.New(v1.Options{Auth: fakeAuthMiddleware(operatorSubject())})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+seededAccount().ID.String(), "x", `{"tier":"pro"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when store unwired", resp.StatusCode)
	}
}

func TestAdminAccountGet_Happy(t *testing.T) {
	acct := seededAccount()
	acct.RateLimitPerMinOverride = 9000
	store := newFakePlatformAccountStore(acct)
	ts := newAdminAccountServer(t, operatorSubject(), store, nil)

	resp, err := http.Get(ts.URL + "/v1/admin/accounts/" + acct.ID.String())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.AdminAccountView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Slug != "acme" || env.Data.RateLimitPerMinOverride != 9000 {
		t.Errorf("view = %+v", env.Data)
	}
}
