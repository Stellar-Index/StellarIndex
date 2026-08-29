// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

func postJSONNoReason(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// api-security-2 (audit 2026-08-28): with SignupRequireEmailVerification
// on (the default), a verified /v1/signup customer who rotated via
// POST /v1/account/keys got a child record with Identifier signup-<hash>
// and ZERO EmailVerifiedAt — RequireEmailVerified then 403'd that child
// forever, since nothing can verify a non-signup KeyID after the fact.

// TestAccountKeysCreate_InheritsEmailVerification — the child carries
// the parent's verification stamp into the store request. Proven red on
// origin/main: CreateAPIKeyRequest had no such field and the child was
// born unverified.
func TestAccountKeysCreate_InheritsEmailVerification(t *testing.T) {
	verifiedAt := time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_child"}, plain: "sip_child"}
	ts := newAccountTestServer(t, auth.Subject{
		Identifier:      "signup-0123456789abcdef",
		Tier:            auth.TierAPIKey,
		KeyID:           "kid_parent",
		EmailVerifiedAt: verifiedAt,
	}, store)

	resp := postJSONNoReason(t, ts.URL+"/v1/account/keys", `{"label":"rotated"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if !store.gotReq.EmailVerifiedAt.Equal(verifiedAt) {
		t.Fatalf("Create.EmailVerifiedAt = %v, want %v (inherited from the verified parent; "+
			"an unverified signup-* child is 403'd by RequireEmailVerified forever)",
			store.gotReq.EmailVerifiedAt, verifiedAt)
	}
}

// TestAccountKeysCreate_UnverifiedParentStaysUnverified — negative pin:
// the stamp is copied, never invented. (The gate blocks an unverified
// signup parent before it reaches this handler; this pins the handler's
// own behaviour independent of the middleware.)
func TestAccountKeysCreate_UnverifiedParentStaysUnverified(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_child"}, plain: "sip_child"}
	ts := newAccountTestServer(t, auth.Subject{
		Identifier: "signup-0123456789abcdef",
		Tier:       auth.TierAPIKey,
		KeyID:      "kid_parent",
	}, store)
	resp := postJSONNoReason(t, ts.URL+"/v1/account/keys", `{"label":"rotated"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if !store.gotReq.EmailVerifiedAt.IsZero() {
		t.Fatalf("Create.EmailVerifiedAt = %v, want zero for an unverified parent", store.gotReq.EmailVerifiedAt)
	}
}
