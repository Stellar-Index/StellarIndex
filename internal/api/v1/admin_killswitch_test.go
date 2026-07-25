// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// C3-010 (audit-2026-07-23) — the operator kill switch.
//
// The suspension machinery existed and was enforced on the read paths,
// but NOTHING in the HTTP surface could trigger it: PATCH
// /v1/admin/accounts/{id} covered tier + the two overrides and never
// Status, and self-service key revoke needs the compromised customer's
// own credential, so staff could not kill a leaked key at all.

// TestAdminAccount_SuspendViaPatch is the account-half regression: an
// operator can move an account to suspended, the suspension bookkeeping
// is written, and it lands in the audit row.
func TestAdminAccount_SuspendViaPatch(t *testing.T) {
	acct := seededAccount()
	store := newFakePlatformAccountStore(acct)
	sink := &recordingAuditSink{}
	ts := newAdminAccountServer(t, auth.Subject{
		Identifier: "ops", Tier: auth.TierOperator, KeyID: "kid_ops",
	}, store, sink)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(),
		"abuse report #4412", `{"status":"suspended","suspended_reason":"credential stuffing from this account"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.AdminAccountView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Status != string(platform.AccountSuspended) {
		t.Errorf("response status = %q, want suspended", env.Data.Status)
	}
	if env.Data.SuspendedReason != "credential stuffing from this account" {
		t.Errorf("response suspended_reason = %q, want the request's reason", env.Data.SuspendedReason)
	}

	if store.updateCalls != 1 {
		t.Fatalf("Update calls = %d, want 1", store.updateCalls)
	}
	got := store.lastUpdate
	if got.Status != platform.AccountSuspended {
		t.Errorf("persisted Status = %q, want suspended — nothing in the HTTP surface could set this pre-fix",
			got.Status)
	}
	if got.SuspendedReason != "credential stuffing from this account" {
		t.Errorf("persisted SuspendedReason = %q, want the request's reason", got.SuspendedReason)
	}
	if got.SuspendedAt.IsZero() {
		t.Error("persisted SuspendedAt is zero — a suspension with no timestamp can't be aged or reviewed")
	}
	// Tier + overrides must be untouched by a status-only patch.
	if got.Tier != acct.Tier {
		t.Errorf("Tier = %q, want %q (unchanged by a status-only patch)", got.Tier, acct.Tier)
	}

	var found bool
	for _, e := range sink.entries {
		if e.Action != "account.override.set" {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(e.Metadata, &meta); err != nil {
			t.Fatalf("audit metadata: %v", err)
		}
		before, _ := meta["before"].(map[string]any)
		after, _ := meta["after"].(map[string]any)
		if before["status"] == "active" && after["status"] == "suspended" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit row does not record the status transition; entries = %+v", sink.entries)
	}
}

// TestAdminAccount_ReactivateClearsSuspension pins the reverse move:
// back to active clears the bookkeeping, so a reactivated account does
// not carry a stale "suspended since / because" forever.
func TestAdminAccount_ReactivateClearsSuspension(t *testing.T) {
	acct := seededAccount()
	acct.Status = platform.AccountSuspended
	acct.SuspendedReason = "abuse"
	acct.SuspendedAt = acct.CreatedAt
	store := newFakePlatformAccountStore(acct)
	ts := newAdminAccountServer(t, auth.Subject{
		Identifier: "ops", Tier: auth.TierOperator, KeyID: "kid_ops",
	}, store, nil)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(),
		"appeal upheld", `{"status":"active"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := store.lastUpdate
	if got.Status != platform.AccountActive {
		t.Errorf("Status = %q, want active", got.Status)
	}
	if !got.SuspendedAt.IsZero() || got.SuspendedReason != "" {
		t.Errorf("reactivation left SuspendedAt=%v SuspendedReason=%q, want both cleared",
			got.SuspendedAt, got.SuspendedReason)
	}
}

// TestAdminAccount_RejectsUnknownStatus pins the vocabulary gate — the
// schema CHECK would reject it anyway, but as a 500 rather than a 400.
func TestAdminAccount_RejectsUnknownStatus(t *testing.T) {
	acct := seededAccount()
	store := newFakePlatformAccountStore(acct)
	ts := newAdminAccountServer(t, auth.Subject{
		Identifier: "ops", Tier: auth.TierOperator, KeyID: "kid_ops",
	}, store, nil)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(),
		"typo", `{"status":"banned"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if store.updateCalls != 0 {
		t.Errorf("Update calls = %d, want 0", store.updateCalls)
	}
}

// TestAdminAccount_StatusRequiresReason pins that the kill switch obeys
// the same X-Reason audit contract as every other admin write.
func TestAdminAccount_StatusRequiresReason(t *testing.T) {
	acct := seededAccount()
	store := newFakePlatformAccountStore(acct)
	ts := newAdminAccountServer(t, auth.Subject{
		Identifier: "ops", Tier: auth.TierOperator, KeyID: "kid_ops",
	}, store, nil)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "", `{"status":"suspended"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (X-Reason required)", resp.StatusCode)
	}
	if store.updateCalls != 0 {
		t.Errorf("Update calls = %d, want 0", store.updateCalls)
	}
}

// ─── operator key revoke ─────────────────────────────────────────

func newAdminKeyServer(t *testing.T, subject auth.Subject, store v1.AccountStore, sink v1.AuditSink) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{
		Auth:     fakeAuthMiddleware(subject),
		Accounts: store,
		Audit:    sink,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func adminDelete(t *testing.T, url, reason string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("NewRequest DELETE %s: %v", url, err)
	}
	if reason != "" {
		req.Header.Set("X-Reason", reason)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestAdminKeysRevoke_KillsALeakedKey is the key-half regression: an
// operator can revoke a credential belonging to somebody else. Pre-fix
// there was no route at all — self-service revoke is scoped to the
// caller's own identifier, so killing a leaked key required the victim's
// credential or a hand-edit of Redis.
func TestAdminKeysRevoke_KillsALeakedKey(t *testing.T) {
	store := &recordingRevokeStore{}
	sink := &recordingAuditSink{}
	ts := newAdminKeyServer(t, auth.Subject{
		Identifier: "ops", Tier: auth.TierOperator, KeyID: "kid_ops",
	}, store, sink)

	resp := adminDelete(t, ts.URL+"/v1/admin/keys/kid_leaked?identifier=signup-victim", "key posted to a public gist")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if store.calls != 1 {
		t.Fatalf("RevokeKeyByID calls = %d, want 1", store.calls)
	}
	if store.identifier != "signup-victim" || store.keyID != "kid_leaked" {
		t.Errorf("revoked (%q, %q), want (signup-victim, kid_leaked)", store.identifier, store.keyID)
	}

	var found bool
	for _, e := range sink.entries {
		if e.Action == "key.revoke" && e.TargetID == "kid_leaked" && e.ActorKind == platform.ActorStaff {
			found = true
		}
	}
	if !found {
		t.Errorf("no key.revoke staff audit row; entries = %+v", sink.entries)
	}
}

// TestAdminKeysRevoke_Guards pins the four refusals: non-operator
// credentials, anonymous callers, a missing X-Reason, and a missing
// owner identifier — none of which may reach the store.
func TestAdminKeysRevoke_Guards(t *testing.T) {
	cases := []struct {
		name    string
		subject auth.Subject
		url     string
		reason  string
		want    int
	}{
		{
			name:    "anonymous",
			subject: auth.Subject{},
			url:     "/v1/admin/keys/kid_x?identifier=signup-a",
			reason:  "r",
			want:    http.StatusUnauthorized,
		},
		{
			name:    "customer tier",
			subject: auth.Subject{Identifier: "signup-a", Tier: auth.TierAPIKey, KeyID: "kid_cust"},
			url:     "/v1/admin/keys/kid_x?identifier=signup-a",
			reason:  "r",
			want:    http.StatusForbidden,
		},
		{
			name:    "missing X-Reason",
			subject: auth.Subject{Identifier: "ops", Tier: auth.TierOperator, KeyID: "kid_ops"},
			url:     "/v1/admin/keys/kid_x?identifier=signup-a",
			reason:  "",
			want:    http.StatusBadRequest,
		},
		{
			name:    "missing identifier",
			subject: auth.Subject{Identifier: "ops", Tier: auth.TierOperator, KeyID: "kid_ops"},
			url:     "/v1/admin/keys/kid_x",
			reason:  "r",
			want:    http.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingRevokeStore{}
			ts := newAdminKeyServer(t, tc.subject, store, nil)
			resp := adminDelete(t, ts.URL+tc.url, tc.reason)
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if store.calls != 0 {
				t.Errorf("store touched %d times on a refused revoke", store.calls)
			}
		})
	}
}

// ─── stubs ───────────────────────────────────────────────────────

// recordingRevokeStore is a v1.AccountStore whose only exercised method
// is RevokeKeyByID; the mint/list halves panic so an accidental call
// surfaces immediately (the fakePlatformAccountsForBridge convention).
type recordingRevokeStore struct {
	calls      int
	identifier string
	keyID      string
	err        error
}

func (s *recordingRevokeStore) Create(_ context.Context, _ auth.CreateAPIKeyRequest) (auth.APIKeyRecord, string, error) {
	panic("unused")
}

func (s *recordingRevokeStore) ListKeysForIdentifier(_ context.Context, _ string) ([]auth.APIKeyRecord, error) {
	panic("unused")
}

func (s *recordingRevokeStore) RevokeKeyByID(_ context.Context, identifier, keyID string) error {
	s.calls++
	s.identifier, s.keyID = identifier, keyID
	return s.err
}
