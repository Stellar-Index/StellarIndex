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

// api-security-1 (audit 2026-08-28): POST /v1/account/keys copied an
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

// TestAccountKeysCreate_OperatorRequiresReason — an operator-tier
// self-mint without X-Reason is refused BEFORE the store is touched,
// mirroring /v1/admin/keys. Proven red on origin/main: the handler
// returned 201 and Create was called once.
func TestAccountKeysCreate_OperatorRequiresReason(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_child"}, plain: "sip_child"}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, operatorSelfSubject(), store, sink)

	resp := doWithReason(t, http.MethodPost, ts.URL+"/v1/account/keys", "", `{"label":"rotate"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (operator self-mint without X-Reason)", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("Create called %d times, want 0 (no reason, no credential)", store.calls)
	}
	if len(sink.entries) != 0 {
		t.Errorf("audit entries = %d, want 0", len(sink.entries))
	}
}

// TestAccountKeysCreate_OperatorSelfMintIsAudited — with X-Reason the
// operator keeps tier inheritance (the documented rotation contract) and
// the mint lands one key.mint audit row naming the actor key, the minted
// key and the reason. Proven red on origin/main: 201 with zero audit
// entries.
func TestAccountKeysCreate_OperatorSelfMintIsAudited(t *testing.T) {
	store := &fakeAccountStore{
		rec:   auth.APIKeyRecord{KeyID: "kid_child", Label: "rotate", Tier: auth.TierOperator},
		plain: "sip_child",
	}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, operatorSelfSubject(), store, sink)

	resp := doWithReason(t, http.MethodPost, ts.URL+"/v1/account/keys", "quarterly rotation", `{"label":"rotate"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if store.gotReq.Tier != auth.TierOperator {
		t.Errorf("Create.Tier = %q, want operator (rotation keeps tier inheritance)", store.gotReq.Tier)
	}
	if store.gotReq.Identifier != "operator:staff-1" {
		t.Errorf("Create.Identifier = %q, want the caller's own identifier", store.gotReq.Identifier)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 key.mint row for an operator self-mint", len(sink.entries))
	}
	e := sink.entries[0]
	if e.Action != "key.mint" || e.ActorKind != platform.ActorStaff ||
		e.TargetKind != "api_key" || e.TargetID != "kid_child" {
		t.Errorf("audit entry = %+v", e)
	}
	for _, want := range []string{`"actor_key_id":"kid_operator1"`, `"reason":"quarterly rotation"`, `"route":"/v1/account/keys"`, `"tier":"operator"`} {
		if !strings.Contains(string(e.Metadata), want) {
			t.Errorf("audit metadata missing %s: %s", want, e.Metadata)
		}
	}
	if e.Timestamp.IsZero() || e.UserAgent == "" {
		t.Errorf("audit entry missing request stamps: ts=%v ua=%q", e.Timestamp, e.UserAgent)
	}
}

// TestAccountKeysCreate_CustomerNeedsNoReason pins the blast radius: a
// customer-tier caller is NOT an admin write — no X-Reason required, no
// staff audit row.
func TestAccountKeysCreate_CustomerNeedsNoReason(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_c"}, plain: "sip_c"}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, auth.Subject{Identifier: "owner-42", Tier: auth.TierAPIKey, KeyID: "kid_owner"}, store, sink)

	resp := doWithReason(t, http.MethodPost, ts.URL+"/v1/account/keys", "", `{"label":"ci"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (customer mint unchanged)", resp.StatusCode)
	}
	if len(sink.entries) != 0 {
		t.Errorf("audit entries = %d, want 0 for a customer self-mint", len(sink.entries))
	}
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
