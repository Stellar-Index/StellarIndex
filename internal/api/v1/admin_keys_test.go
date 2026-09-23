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

	"github.com/google/uuid"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// The audit sink test double is keybudgets_fakes_test.go's
// recordingAuditSink — same package, same AuditSink shape.

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

// newAdminTestServerWithPlatformKeys additionally wires a Postgres
// [platform.APIKeyStore] behind APIKeyBudgets.Platform plus the account
// store that proves a row's owner — needed for the GH-978
// revoke-both-stores regressions, where the shared credential has a
// durable management row alongside its Redis record.
func newAdminTestServerWithPlatformKeys(
	t *testing.T, subject auth.Subject, store v1.AccountStore,
	platformKeys platform.APIKeyStore, platformAccounts v1.PlatformAccountStore,
) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{
		Auth:             fakeAuthMiddleware(subject),
		Accounts:         store,
		APIKeyBudgets:    v1.APIKeyBudgetStores{Platform: platformKeys},
		PlatformAccounts: platformAccounts,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// postJSON POSTs with a stock X-Reason. Every admin write requires the
// header (platform-spec §7.2); use status_notices_test.go's
// postJSONWithReason directly to exercise the missing-header path.
func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	return postJSONWithReason(t, url, "test mint", body)
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

// TestAdminKeysCreate_RequiresReason pins C3-107 (audit-2026-07-23).
// Minting a privileged credential is at least as consequential as
// setting a per-account override or killing a key, both of which hard-400
// without an X-Reason header. The mint was the one admin write that
// captured no reason: the audit row recorded WHO minted WHAT, never WHY.
func TestAdminKeysCreate_RequiresReason(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_x"}, plain: "p"}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, operatorSubject(), store, sink)

	resp := postJSONWithReason(t, ts.URL+"/v1/admin/keys", "",
		`{"identifier":"acct:partner-co","label":"l"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 without X-Reason (same contract as "+
			"PATCH /v1/admin/accounts and DELETE /v1/admin/keys/{keyID})", resp.StatusCode)
	}
	// Fail BEFORE the mint — a key that exists with no recorded reason is
	// exactly the state the header is there to prevent.
	if store.calls != 0 {
		t.Errorf("store.Create called %d times despite the missing X-Reason — the key was minted anyway", store.calls)
	}
	if len(sink.entries) != 0 {
		t.Errorf("audit entries = %d, want 0", len(sink.entries))
	}
}

// TestAdminKeysCreate_ReasonReachesAuditRow — the header is not just a
// gate: the reason must land in the persisted audit row, or requiring it
// buys nothing an operator can read back later.
func TestAdminKeysCreate_ReasonReachesAuditRow(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_minted01"}, plain: "p"}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, operatorSubject(), store, sink)

	const reason = "partner onboarding SI-4412"
	resp := postJSONWithReason(t, ts.URL+"/v1/admin/keys", reason,
		`{"identifier":"acct:partner-co","label":"l"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(sink.entries))
	}
	if !strings.Contains(string(sink.entries[0].Metadata), reason) {
		t.Errorf("audit metadata = %s, want it to carry the X-Reason %q",
			sink.entries[0].Metadata, reason)
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

// seedOwnedPlatformKey seeds one Postgres api_keys row owned by the
// account whose slug is ownerSlug and returns both stores behind it.
func seedOwnedPlatformKey(ownerSlug, keyID string) (*fakeRegisterKeyStore, *fakePlatformAccountStore) {
	owner := platform.Account{ID: uuid.New(), Slug: ownerSlug}
	keys := newFakeRegisterKeyStore()
	keys.byID[keyID] = platform.APIKey{ID: keyID, AccountID: owner.ID}
	return keys, newFakePlatformAccountStore(owner)
}

func assertPlatformKeyLive(t *testing.T, keys *fakeRegisterKeyStore, keyID string) {
	t.Helper()
	if len(keys.revokedIDs) != 0 || !keys.byID[keyID].RevokedAt.IsZero() {
		t.Errorf("another account's api_keys row was revoked: revokedIDs = %v, RevokedAt = %v — "+
			"the Postgres leg must be scoped to the identifier's own keys (GH-978)",
			keys.revokedIDs, keys.byID[keyID].RevokedAt)
	}
}

// TestAdminKeysRevoke_RevokesPostgresManagementRowToo is the GH-978
// regression: DELETE /v1/admin/keys/{kid} must revoke the credential
// in BOTH stores a /v1/register-minted key lives in, not just Redis —
// otherwise the api_keys row keeps revoked_at NULL, stays listed and
// counts toward the active-key ceiling. The row belongs to the
// account behind identifier=acct:reg-abc123.
func TestAdminKeysRevoke_RevokesPostgresManagementRowToo(t *testing.T) {
	platformKeys, accts := seedOwnedPlatformKey("reg-abc123", "kid_shared01")
	ts := newAdminTestServerWithPlatformKeys(t, operatorSubject(), &fakeAccountStore{}, platformKeys, accts)

	resp := doWithReason(t, http.MethodDelete,
		ts.URL+"/v1/admin/keys/kid_shared01?identifier=acct:reg-abc123", "leaked key", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	if len(platformKeys.revokedIDs) != 1 || platformKeys.revokedIDs[0] != "kid_shared01" {
		t.Errorf("postgres management row not revoked: revokedIDs = %v, want [kid_shared01] — "+
			"the row stays revoked_at NULL, still counted active and listed (GH-978)", platformKeys.revokedIDs)
	}
}

// TestAdminKeysRevoke_NonOwnerIdentifierLeavesPostgresRowLive pins the
// route's identifier=<owner> contract on the Postgres leg: Revoke is
// keyed by id alone, so naming the wrong owner must not reach another
// account's row. 204 either way, so key ids can't be enumerated.
func TestAdminKeysRevoke_NonOwnerIdentifierLeavesPostgresRowLive(t *testing.T) {
	platformKeys, accts := seedOwnedPlatformKey("victim-co", "kid_victim01")
	ts := newAdminTestServerWithPlatformKeys(t, operatorSubject(), &fakeAccountStore{}, platformKeys, accts)

	resp := doWithReason(t, http.MethodDelete,
		ts.URL+"/v1/admin/keys/kid_victim01?identifier=acct:attacker-co", "leaked key", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (a non-owner revoke is a silent no-op)", resp.StatusCode)
	}
	assertPlatformKeyLive(t, platformKeys, "kid_victim01")
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

// TestAdminKeysRevoke_NoAccountStoreLeavesPostgresRowLive: with no
// account store the owner can't be proven, so the Postgres leg fails
// closed rather than revoking by id alone.
func TestAdminKeysRevoke_NoAccountStoreLeavesPostgresRowLive(t *testing.T) {
	platformKeys, _ := seedOwnedPlatformKey("victim-co", "kid_victim01")
	ts := newAdminTestServerWithPlatformKeys(t, operatorSubject(), &fakeAccountStore{}, platformKeys, nil)

	resp := doWithReason(t, http.MethodDelete,
		ts.URL+"/v1/admin/keys/kid_victim01?identifier=acct:attacker-co", "leaked key", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	assertPlatformKeyLive(t, platformKeys, "kid_victim01")
}

// TestAdminKeysRevoke_TolerantOfRedisOnlyKey pins the other half: a
// key that was minted through an admin/self-service path (Redis
// only, no Postgres row) must still revoke cleanly — the Postgres
// leg is best-effort and ErrNotFound there is not a failure.
func TestAdminKeysRevoke_TolerantOfRedisOnlyKey(t *testing.T) {
	platformKeys := newFakeRegisterKeyStore() // no row for kid_redisonly
	ts := newAdminTestServerWithPlatformKeys(t, operatorSubject(), &fakeAccountStore{}, platformKeys,
		newFakePlatformAccountStore(platform.Account{ID: uuid.New(), Slug: "partner-co"}))

	resp := doWithReason(t, http.MethodDelete,
		ts.URL+"/v1/admin/keys/kid_redisonly?identifier=acct:partner-co", "leaked key", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (a Redis-only key with no Postgres row must still revoke)", resp.StatusCode)
	}
}
