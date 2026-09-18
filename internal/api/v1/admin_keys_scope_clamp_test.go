// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// middleware.ClampMintScopes' own doc calls itself "the single
// chokepoint every mint path funnels through" so that "a credential can
// never mint a more-privileged credential than itself" holds by
// construction. That was true of the CUSTOMER path only: POST
// /v1/admin/keys gated on tier alone and passed the requested scopes
// straight into the store (security review A-2, 2026-09-17).
//
// The escalation is closed, not theoretical: this very handler mints
// `tier: operator` keys WITH an explicit scope list, so a narrowed
// operator credential exists by construction — and an empty scope list
// means every capability (checkScopes short-circuits on
// len(Scopes)==0). A staff key minted as operator/["admin"] —
// deliberately able to drive /v1/admin/* but not to read customer data —
// could POST scopes:[] here and mint itself full access, with a
// best-effort audit row as the only signal.
//
// Both directions are asserted on the STORE's received request, not on
// the status code: a 201 that persists the wrong scope set is the bug.

// narrowedOperatorSubject is an operator credential confined to the
// /v1/admin/* family — the shape the admin mint path itself can issue.
func narrowedOperatorSubject() auth.Subject {
	s := operatorSubject()
	s.Scopes = []string{platform.KeyScopeAdmin}
	return s
}

// A scoped caller asking for nothing must NOT get everything: the child
// inherits the parent's own scopes rather than defaulting to full
// access.
func TestAdminKeysCreate_NarrowedOperatorCannotMintUnscopedKey(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_minted01"}, plain: "sip_x"}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, narrowedOperatorSubject(), store, sink)

	resp := postJSON(t, ts.URL+"/v1/admin/keys",
		`{"identifier":"acct:self","label":"escalation","tier":"operator","scopes":[]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (the clamp narrows, it does not reject an empty request)", resp.StatusCode)
	}
	if store.calls != 1 {
		t.Fatalf("store.Create calls = %d, want 1", store.calls)
	}
	got := store.gotReq.Scopes
	if len(got) != 1 || got[0] != platform.KeyScopeAdmin {
		t.Fatalf("minted Scopes = %v, want [%q] inherited from the caller — an empty list is FULL ACCESS, "+
			"so a narrowed operator key just escalated itself", got, platform.KeyScopeAdmin)
	}
}

// A scoped caller asking for a scope it does not hold is rejected
// outright — not silently narrowed, and never minted.
func TestAdminKeysCreate_NarrowedOperatorCannotMintScopeItLacks(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_minted02"}, plain: "sip_x"}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, narrowedOperatorSubject(), store, sink)

	resp := postJSON(t, ts.URL+"/v1/admin/keys",
		`{"identifier":"acct:partner-co","label":"data-reader","scopes":["read"]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — %q is outside the caller's own scopes", resp.StatusCode, platform.KeyScopeRead)
	}
	if store.calls != 0 {
		t.Fatalf("store.Create called %d times despite the scope refusal — the key was minted anyway", store.calls)
	}
	if len(sink.entries) != 0 {
		t.Fatalf("audit entries = %d, want 0 — nothing was minted", len(sink.entries))
	}
}

// Regression guard for the pre-scopes posture: a full-access operator
// (empty scope list) still delegates freely, including minting another
// full-access key. The clamp must not break staff onboarding.
func TestAdminKeysCreate_FullAccessOperatorStillDelegatesFreely(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_minted03"}, plain: "sip_x"}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, operatorSubject(), store, sink)

	resp := postJSON(t, ts.URL+"/v1/admin/keys",
		`{"identifier":"acct:partner-co","label":"full","scopes":[]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if len(store.gotReq.Scopes) != 0 {
		t.Fatalf("minted Scopes = %v, want the empty (full-access) list an unscoped operator may still delegate",
			store.gotReq.Scopes)
	}
}
