// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// POST /v1/account/keys copies an operator caller's tier into the child (documented intent,
// for staff rotation), so it must carry the admin mint audit contract: an X-Reason and a
// key.mint audit row. Otherwise a compromised staff credential could spawn further operator
// credentials that POST /v1/admin/keys would have refused without a reason and logged.

func operatorSelfSubject() auth.Subject {
	return auth.Subject{
		Identifier: "operator:staff-1",
		Tier:       auth.TierOperator,
		KeyID:      "kid_operator1",
		// As the validators surface a full-access key; a closed-posture minter may mint nothing.
		AllowAllPermissions: true,
	}
}

func doWithReason(t *testing.T, method, url, reason, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest %s %s: %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if reason != "" {
		req.Header.Set("X-Reason", reason)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestAccountKeysRevoke_OperatorRequiresReasonAndAudits mirrors the
// mint contract on DELETE /v1/account/keys/{keyID}: an operator revoke
// without X-Reason is 400; with it, 204 plus one key.revoke row.
func TestAccountKeysRevoke_OperatorRequiresReasonAndAudits(t *testing.T) {
	store := &fakeAccountStore{}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, operatorSelfSubject(), store, sink)

	resp := doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/kid_other", "", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (operator revoke without X-Reason)", resp.StatusCode)
	}
	if len(sink.entries) != 0 {
		t.Fatalf("audit entries = %d, want 0", len(sink.entries))
	}

	resp = doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/kid_other", "leaked in CI log", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 key.revoke row", len(sink.entries))
	}
	e := sink.entries[0]
	if e.Action != "key.revoke" || e.ActorKind != platform.ActorStaff || e.TargetID != "kid_other" {
		t.Errorf("audit entry = %+v", e)
	}
	if !strings.Contains(string(e.Metadata), `"reason":"leaked in CI log"`) {
		t.Errorf("audit metadata missing reason: %s", e.Metadata)
	}
}

// TestAccountKeysRevoke_CustomerNeedsNoReason — a customer revoke needs no header and lands one
// user-actor key.revoke row.
func TestAccountKeysRevoke_CustomerNeedsNoReason(t *testing.T) {
	sink := &recordingAuditSink{}
	store := &fakeAccountStore{listed: map[string][]auth.APIKeyRecord{
		"owner-42": {{KeyID: "kid_other", Identifier: "owner-42", ParentKeyID: "kid_owner"}},
	}}
	ts := newAdminTestServer(t, auth.Subject{Identifier: "owner-42", Tier: auth.TierAPIKey, KeyID: "kid_owner"}, store, sink)
	resp := doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/kid_other", "", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 key.revoke row", len(sink.entries))
	}
	if e := sink.entries[0]; e.Action != "key.revoke" || e.ActorKind != platform.ActorUser || e.TargetID != "kid_other" {
		t.Errorf("audit entry = %+v, want a user-actor key.revoke row for kid_other", e)
	}
}

// TestAccountKeysRevoke_CrossAccountLeavesPostgresRowLive: a customer's DELETE /v1/account/keys/{kid} with another account's
// key id must not revoke that account's api_keys row.
func TestAccountKeysRevoke_CrossAccountLeavesPostgresRowLive(t *testing.T) {
	platformKeys, accts := seedOwnedPlatformKey("victim-co", "kid_victim01")
	attacker := auth.Subject{Identifier: "acct:attacker-co", Tier: auth.TierAPIKey, KeyID: "kid_attacker"}
	ts := newAdminTestServerWithPlatformKeys(t, attacker, &fakeAccountStore{}, platformKeys, accts)

	resp := doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/kid_victim01", "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (another account's key is not delegated from the caller)", resp.StatusCode)
	}
	assertPlatformKeyLive(t, platformKeys, "kid_victim01")
}

// TestAccountKeysRevoke_RootKeyCannotRevokeAnotherRootsRow: a management row (register or dashboard key) is a root, so
// no other API key on the account can revoke it here; a member's key would otherwise revoke the owner's. The
// dashboard path, which knows the user's role, revokes it.
func TestAccountKeysRevoke_RootKeyCannotRevokeAnotherRootsRow(t *testing.T) {
	platformKeys, accts := seedOwnedPlatformKey("reg-abc123", "kid_shared01")
	member := auth.Subject{Identifier: "acct:reg-abc123", Tier: auth.TierAPIKey, KeyID: "kid_other"}
	ts := newAdminTestServerWithPlatformKeys(t, member, &fakeAccountStore{}, platformKeys, accts)

	resp := doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/kid_shared01", "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	assertPlatformKeyLive(t, platformKeys, "kid_shared01")
}

// A plaintext key pasted where a key id belongs is refused on both revoke routes: nothing is looked up, the answer
// says nothing was revoked, and the secret is not echoed back.
func TestKeyRevoke_RefusesAPlaintextKeyAsTheID(t *testing.T) {
	secret := "sip_" + strings.Repeat("ab", 32)
	for name, tc := range map[string]struct {
		subject auth.Subject
		path    string
	}{
		"self-service": {subject: auth.Subject{Identifier: "owner-42", Tier: auth.TierAPIKey, KeyID: "kid_owner"}, path: "/v1/account/keys/" + secret},
		"operator":     {subject: operatorSelfSubject(), path: "/v1/admin/keys/" + secret + "?identifier=signup-victim"},
		// The escaped spelling decodes to the same path value and must not slip past the body's redaction.
		"self-service, percent-encoded": {
			subject: auth.Subject{Identifier: "owner-42", Tier: auth.TierAPIKey, KeyID: "kid_owner"},
			path:    "/v1/account/keys/sip%5F" + secret[4:],
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeAccountStore{}
			sink := &recordingAuditSink{}
			ts := newAdminTestServer(t, tc.subject, store, sink)
			resp := doWithReason(t, http.MethodDelete, ts.URL+tc.path, "leaked", "")
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: a 204 or 404 reads as revoked or already gone", resp.StatusCode)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "key-id-is-a-secret") {
				t.Errorf("problem does not name the mistake: %s", body)
			}
			if strings.Contains(string(body), secret[8:]) {
				t.Errorf("the answer echoes the plaintext key: %s", body)
			}
			if store.revokeCalls != 0 || len(sink.entries) != 0 {
				t.Errorf("revoke calls = %d, audit rows = %d; want none", store.revokeCalls, len(sink.entries))
			}
		})
	}
}
