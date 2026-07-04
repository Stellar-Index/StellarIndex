// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
	"github.com/StellarIndex/stellar-index/internal/auth"
	"github.com/StellarIndex/stellar-index/internal/platform"
)

// The audit sink test double is stripe_webhook_test.go's
// recordingAuditSink — same package, same StripeAuditSink/AuditSink
// shape.

func newAdminTestServer(t *testing.T, subject auth.Subject, store v1.AccountStore, sink v1.AuditSink) *httptest.Server {
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

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func operatorSubject() auth.Subject {
	return auth.Subject{
		Identifier: "operator:staff-1",
		Tier:       auth.TierOperator,
		KeyID:      "kid_operator1",
	}
}

// TestAdminKeysCreate_Happy pins the operator mint path: the target
// identifier/tier/scopes come from the request body (NOT inherited
// from the caller), and the mint lands one "key.mint" audit row.
func TestAdminKeysCreate_Happy(t *testing.T) {
	store := &fakeAccountStore{
		rec: auth.APIKeyRecord{
			KeyID:     "kid_minted01",
			KeyPrefix: "sip_deadbeef",
			Label:     "partner-integration",
			Scopes:    []string{"read"},
			CreatedAt: time.Unix(1751000000, 0).UTC(),
		},
		plain: "sip_deadbeefcafe",
	}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, operatorSubject(), store, sink)

	resp := postJSON(t, ts.URL+"/v1/admin/keys",
		`{"identifier":"acct:partner-co","label":"partner-integration","scopes":["read"],"rate_limit_per_min":5000}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	// The store must receive the TARGET identifier + explicit fields.
	if store.gotReq.Identifier != "acct:partner-co" {
		t.Errorf("Identifier = %q, want the request-body target", store.gotReq.Identifier)
	}
	if store.gotReq.Tier != auth.TierAPIKey {
		t.Errorf("Tier = %q, want default apikey", store.gotReq.Tier)
	}
	if store.gotReq.RateLimitPerMin != 5000 {
		t.Errorf("RateLimitPerMin = %d", store.gotReq.RateLimitPerMin)
	}
	if len(store.gotReq.Scopes) != 1 || store.gotReq.Scopes[0] != "read" {
		t.Errorf("Scopes = %v", store.gotReq.Scopes)
	}

	var env struct {
		Data v1.KeyCreated `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Plaintext != "sip_deadbeefcafe" || env.Data.KeyID != "kid_minted01" {
		t.Errorf("KeyCreated = %+v", env.Data)
	}

	// Audit row: action key.mint, staff actor, minted key as target.
	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(sink.entries))
	}
	e := sink.entries[0]
	if e.Action != "key.mint" || e.ActorKind != platform.ActorStaff ||
		e.TargetKind != "api_key" || e.TargetID != "kid_minted01" {
		t.Errorf("audit entry = %+v", e)
	}
	if !strings.Contains(string(e.Metadata), "acct:partner-co") {
		t.Errorf("audit metadata missing target identifier: %s", e.Metadata)
	}
}

func TestAdminKeysCreate_NonOperator403(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "x"}, plain: "y"}
	ts := newAdminTestServer(t, auth.Subject{
		Identifier: "acct:customer", Tier: auth.TierAPIKey, KeyID: "kid_cust",
	}, store, nil)

	resp := postJSON(t, ts.URL+"/v1/admin/keys",
		`{"identifier":"acct:victim","label":"nope"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for non-operator", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("store.Create called %d times by a non-operator", store.calls)
	}
}

func TestAdminKeysCreate_Anonymous401(t *testing.T) {
	ts := newAdminTestServer(t, auth.Subject{}, &fakeAccountStore{}, nil)
	resp := postJSON(t, ts.URL+"/v1/admin/keys", `{"identifier":"acct:x","label":"l"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAdminKeysCreate_Validation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing identifier", `{"label":"l"}`},
		{"missing label", `{"identifier":"acct:x"}`},
		{"bad tier", `{"identifier":"acct:x","label":"l","tier":"sep10"}`},
		{"bad scope", `{"identifier":"acct:x","label":"l","scopes":["everything"]}`},
		{"bad rate limit", `{"identifier":"acct:x","label":"l","rate_limit_per_min":-1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeAccountStore{}
			ts := newAdminTestServer(t, operatorSubject(), store, nil)
			resp := postJSON(t, ts.URL+"/v1/admin/keys", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			if store.calls != 0 {
				t.Errorf("store.Create called on invalid input")
			}
		})
	}
}

// TestAccountKeysCreate_WithScopes pins the self-service scope
// plumbing: valid scopes flow into CreateAPIKeyRequest (deduped),
// unknown scopes 400 before touching the store.
func TestAccountKeysCreate_WithScopes(t *testing.T) {
	subject := auth.Subject{Identifier: "cust-42", Tier: auth.TierAPIKey, KeyID: "kid_a"}
	store := &fakeAccountStore{
		rec:   auth.APIKeyRecord{KeyID: "kid_new", Scopes: []string{"read"}},
		plain: "sip_plain",
	}
	ts := newAccountTestServer(t, subject, store)

	resp := postJSON(t, ts.URL+"/v1/account/keys",
		`{"label":"ci-bot","scopes":["read","read"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if len(store.gotReq.Scopes) != 1 || store.gotReq.Scopes[0] != "read" {
		t.Errorf("Scopes = %v, want deduped [read]", store.gotReq.Scopes)
	}
	var env struct {
		Data v1.KeyCreated `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data.Scopes) != 1 || env.Data.Scopes[0] != "read" {
		t.Errorf("response scopes = %v", env.Data.Scopes)
	}

	// Unknown scope → 400, store untouched.
	store2 := &fakeAccountStore{}
	ts2 := newAccountTestServer(t, subject, store2)
	resp2 := postJSON(t, ts2.URL+"/v1/account/keys",
		`{"label":"ci-bot","scopes":["everything"]}`)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown scope", resp2.StatusCode)
	}
	if store2.calls != 0 {
		t.Errorf("store.Create called despite invalid scope")
	}
}
