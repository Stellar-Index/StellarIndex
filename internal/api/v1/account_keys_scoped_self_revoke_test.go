// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// newScopedRevokeServer serves tr's keys through the production Auth and KeyPolicy middleware, not an injected subject.
func newScopedRevokeServer(t *testing.T, tr *revokeTree, sink v1.AuditSink) *httptest.Server {
	t.Helper()
	tr.mint(t, "readonly", auth.ChildKeyRequest(tr.subjects["owner"], "readonly", []string{"read"}))
	pinned := auth.ChildKeyRequest(tr.subjects["owner"], "pinned", []string{"read"})
	pinned.Restrictions.IPAllowlist = []string{"10.0.0.0/8"}
	tr.mint(t, "pinned", pinned)
	srv := v1.New(v1.Options{
		Auth:      middleware.Auth(middleware.AuthOptions{Mode: middleware.AuthModeAPIKey, APIKey: tr.validator}),
		KeyPolicy: middleware.KeyPolicy(),
		Accounts:  tr.store,
		Audit:     sink,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// callAs sends method path presenting actor's plaintext key and returns the status and the problem type.
func callAs(t *testing.T, ts *httptest.Server, tr *revokeTree, actor, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(middleware.HeaderAPIKey, tr.plains[actor])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, string(body)
}

// A key scoped away from `account` revokes itself, with the same audit row an unscoped key leaves, and is dead after.
func TestAccountKeysRevoke_ScopedKeyRevokesItself(t *testing.T) {
	tr := newRevokeTree(t)
	sink := &recordingAuditSink{}
	ts := newScopedRevokeServer(t, tr, sink)
	self := tr.subjects["readonly"].KeyID

	if got, body := callAs(t, ts, tr, "readonly", http.MethodDelete, "/v1/account/keys/"+self); got != http.StatusNoContent {
		t.Fatalf("scoped self-revoke = %d %s, want 204", got, body)
	}
	if tr.live("readonly") {
		t.Error("readonly still authenticates after revoking itself")
	}
	if len(sink.entries) != 1 || sink.entries[0].Action != "key.revoke" || sink.entries[0].TargetID != self ||
		!strings.Contains(string(sink.entries[0].Metadata), `"actor_key_id":"`+self+`"`) {
		t.Errorf("audit entries = %+v, want one key.revoke of %s naming itself as actor", sink.entries, self)
	}
	for _, name := range []string{"owner", "child", "sibling", "pinned"} {
		if !tr.live(name) {
			t.Errorf("%s was revoked by readonly's self-revoke", name)
		}
	}
	if got, _ := callAs(t, ts, tr, "readonly", http.MethodDelete, "/v1/account/keys/"+self); got != http.StatusUnauthorized {
		t.Errorf("revoked key retrying its revoke = %d, want 401", got)
	}
}

// The exemption names one route and one target: every other key-management call stays outside a read-scoped key, and
// the key's IP allowlist still binds the self-revoke.
func TestAccountKeysRevoke_ScopedKeyGainsNothingElse(t *testing.T) {
	tr := newRevokeTree(t)
	sink := &recordingAuditSink{}
	ts := newScopedRevokeServer(t, tr, sink)
	self := tr.subjects["readonly"].KeyID
	cases := []struct {
		name, actor, method, path, problem string
	}{
		{"revoke a sibling", "readonly", http.MethodDelete, "/v1/account/keys/" + tr.subjects["sibling"].KeyID, "scope-denied"},
		{"revoke its minter", "readonly", http.MethodDelete, "/v1/account/keys/" + tr.subjects["owner"].KeyID, "scope-denied"},
		{"list keys", "readonly", http.MethodGet, "/v1/account/keys", "scope-denied"},
		{"mint a key", "readonly", http.MethodPost, "/v1/account/keys", "scope-denied"},
		{"other method on its own path", "readonly", http.MethodPost, "/v1/account/keys/" + self, "scope-denied"},
		{"admin revoke of itself", "readonly", http.MethodDelete, "/v1/admin/keys/" + self, "scope-denied"},
		{
			"self-revoke from outside its allowlist", "pinned", http.MethodDelete,
			"/v1/account/keys/" + tr.subjects["pinned"].KeyID, "ip-not-allowed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, body := callAs(t, ts, tr, tc.actor, tc.method, tc.path)
			if got != http.StatusForbidden || !strings.Contains(body, "/errors/"+tc.problem) {
				t.Errorf("status = %d %s, want 403 %s", got, body, tc.problem)
			}
		})
	}
	if len(sink.entries) != 0 {
		t.Errorf("refused calls wrote audit rows: %+v", sink.entries)
	}
	for name := range tr.plains {
		if !tr.live(name) {
			t.Errorf("%s was revoked by a refused call", name)
		}
	}
}

// An unscoped key keeps the reach it had: it lists keys, revokes a key it minted, and revokes itself.
func TestAccountKeysRevoke_UnscopedKeyUnchangedThroughKeyPolicy(t *testing.T) {
	tr := newRevokeTree(t)
	sink := &recordingAuditSink{}
	ts := newScopedRevokeServer(t, tr, sink)
	if got, body := callAs(t, ts, tr, "owner", http.MethodGet, "/v1/account/keys"); got != http.StatusOK {
		t.Errorf("unscoped list = %d %s, want 200", got, body)
	}
	if got, body := callAs(t, ts, tr, "owner", http.MethodDelete, "/v1/account/keys/"+tr.subjects["child"].KeyID); got != http.StatusNoContent {
		t.Errorf("unscoped revoke of its child = %d %s, want 204", got, body)
	}
	if got, body := callAs(t, ts, tr, "owner", http.MethodDelete, "/v1/account/keys/"+tr.subjects["owner"].KeyID); got != http.StatusNoContent {
		t.Errorf("unscoped self-revoke = %d %s, want 204", got, body)
	}
	if len(sink.entries) != 2 {
		t.Errorf("audit entries = %d, want 2 key.revoke rows", len(sink.entries))
	}
}

// selfRevokeNoRedirect sends DELETE path as actor with extra headers, without following redirects.
func selfRevokeNoRedirect(t *testing.T, ts *httptest.Server, tr *revokeTree, actor, path string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set(middleware.HeaderAPIKey, tr.plains[actor])
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read DELETE %s: %v", path, err)
	}
	return resp.StatusCode, string(body)
}

// The exemption covers scopes only: the Referer allow-list, the endpoint permission list, the path match and the
// operator-tier X-Reason rule all still bind a scoped key revoking itself, and each refusal leaves the key live.
func TestAccountKeysRevoke_ScopedSelfRevokeStillBoundByEveryOtherGate(t *testing.T) {
	tr := newRevokeTree(t)
	sink := &recordingAuditSink{}
	ts := newScopedRevokeServer(t, tr, sink)

	refPinned := auth.ChildKeyRequest(tr.subjects["owner"], "refpinned", []string{"read"})
	refPinned.Restrictions.RefererAllowlist = []string{"app.example.com"}
	tr.mint(t, "refpinned", refPinned)

	noRoute := auth.ChildKeyRequest(tr.subjects["owner"], "noroute", []string{"read"})
	noRoute.Restrictions.PermissionsAll = false
	noRoute.Restrictions.AllowPermissions = []auth.SubjectPermissionEntry{{EndpointPrefix: "/v1/prices"}}
	tr.mint(t, "noroute", noRoute)

	tr.mint(t, "oproot", auth.CreateAPIKeyRequest{Identifier: "acct:op", Tier: auth.TierOperator})
	tr.mint(t, "opscoped", auth.ChildKeyRequest(tr.subjects["oproot"], "opscoped", []string{"read"}))

	own := func(name string) string { return "/v1/account/keys/" + tr.subjects[name].KeyID }
	cases := []struct {
		name, actor, path, problem string
		wantStatus                 int
		headers                    map[string]string
	}{
		{
			"wrong referer", "refpinned", own("refpinned"), "referer-not-allowed", http.StatusForbidden,
			map[string]string{"Referer": "https://evil.example.net/x"},
		},
		{"missing referer", "refpinned", own("refpinned"), "referer-not-allowed", http.StatusForbidden, nil},
		{"permission list omits the route", "noroute", own("noroute"), "permission-denied", http.StatusForbidden, nil},
		{"operator-tier without X-Reason", "opscoped", own("opscoped"), "missing-reason", http.StatusBadRequest, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, body := selfRevokeNoRedirect(t, ts, tr, tc.actor, tc.path, tc.headers)
			if got != tc.wantStatus || !strings.Contains(body, "/errors/"+tc.problem) {
				t.Errorf("status = %d %s, want %d %s", got, body, tc.wantStatus, tc.problem)
			}
			if !tr.live(tc.actor) {
				t.Errorf("%s was revoked by a refused self-revoke", tc.actor)
			}
		})
	}

	t.Run("unclean path", func(t *testing.T) {
		got, body := selfRevokeNoRedirect(t, ts, tr, "readonly", "/v1/account/keys/../keys/"+tr.subjects["readonly"].KeyID, nil)
		if got != http.StatusForbidden || !strings.Contains(body, "/errors/scope-denied") {
			t.Errorf("unclean path self-revoke = %d %s, want 403 scope-denied", got, body)
		}
		if !tr.live("readonly") {
			t.Error("readonly was revoked through an unclean path")
		}
	})

	if len(sink.entries) != 0 {
		t.Errorf("refused calls wrote audit rows: %+v", sink.entries)
	}
}
