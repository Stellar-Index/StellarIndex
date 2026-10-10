// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// POST /v1/account/keys copied an
// operator caller's tier verbatim into the child and recorded nothing —
// no X-Reason, no key.mint audit row — so a compromised staff credential
// could spawn further operator credentials that the admin mint contract
// (POST /v1/admin/keys) would have refused without a reason and logged.
// Tier inheritance is documented intent (staff rotation); the audit
// contract is what was missing.

func operatorSelfSubject() auth.Subject {
	return auth.Subject{
		Identifier: "operator:staff-1",
		Tier:       auth.TierOperator,
		KeyID:      "kid_operator1",
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

// TestAccountKeysRevoke_CustomerNeedsNoReason — customer revoke path
// unchanged (no header, 204, no staff row).
func TestAccountKeysRevoke_CustomerNeedsNoReason(t *testing.T) {
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, auth.Subject{Identifier: "owner-42", Tier: auth.TierAPIKey, KeyID: "kid_owner"}, &fakeAccountStore{}, sink)
	resp := doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/kid_other", "", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if len(sink.entries) != 0 {
		t.Errorf("audit entries = %d, want 0", len(sink.entries))
	}
}

// TestAccountKeysRevoke_CrossAccountLeavesPostgresRowLive is the
// self-service form of the same check: a customer's DELETE
// /v1/account/keys/{kid} with another account's key id in the path
// must not revoke that account's api_keys row.
func TestAccountKeysRevoke_CrossAccountLeavesPostgresRowLive(t *testing.T) {
	platformKeys, accts := seedOwnedPlatformKey("victim-co", "kid_victim01")
	attacker := auth.Subject{Identifier: "acct:attacker-co", Tier: auth.TierAPIKey, KeyID: "kid_attacker"}
	ts := newAdminTestServerWithPlatformKeys(t, attacker, &fakeAccountStore{}, platformKeys, accts)

	resp := doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/kid_victim01", "", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (a non-owner revoke is a silent no-op)", resp.StatusCode)
	}
	assertPlatformKeyLive(t, platformKeys, "kid_victim01")
}

// TestAccountKeysRevoke_OwnerRevokesPostgresRow is the positive
// self-service case: the owner's revoke clears the management row.
func TestAccountKeysRevoke_OwnerRevokesPostgresRow(t *testing.T) {
	platformKeys, accts := seedOwnedPlatformKey("reg-abc123", "kid_shared01")
	owner := auth.Subject{Identifier: "acct:reg-abc123", Tier: auth.TierAPIKey, KeyID: "kid_other"}
	ts := newAdminTestServerWithPlatformKeys(t, owner, &fakeAccountStore{}, platformKeys, accts)

	resp := doWithReason(t, http.MethodDelete, ts.URL+"/v1/account/keys/kid_shared01", "", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if len(platformKeys.revokedIDs) != 1 || platformKeys.revokedIDs[0] != "kid_shared01" {
		t.Errorf("owner's postgres management row not revoked: revokedIDs = %v, want [kid_shared01]", platformKeys.revokedIDs)
	}
}
