// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// The audit sink test double is keybudgets_fakes_test.go's
// recordingAuditSink — same package, same AuditSink shape.

// adminTestAccountSlugs are the platform accounts newAdminTestServer
// knows, so an acct:<slug> mint in these tests passes the existence check.
var adminTestAccountSlugs = []string{"partner-co", "metered-co", "target", "ok", "self", "x"}

func newAdminTestServer(t *testing.T, subject auth.Subject, store v1.AccountStore, sink v1.AuditSink) *httptest.Server {
	t.Helper()
	accounts := &fakePlatformAccountStore{byID: map[uuid.UUID]platform.Account{}}
	for _, slug := range adminTestAccountSlugs {
		id := uuid.New()
		accounts.byID[id] = platform.Account{ID: id, Slug: slug}
	}
	srv := v1.New(v1.Options{
		Auth:             fakeAuthMiddleware(subject),
		Accounts:         store,
		Audit:            sink,
		PlatformAccounts: accounts,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// newAdminTestServerWithPlatformKeys additionally wires a Postgres
// [platform.APIKeyStore] behind APIKeyBudgets.Platform plus the account
// store that proves a row's owner — needed for the
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
// header; use status_notices_test.go's
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
		`{"identifier":"acct:partner-co","account":"partner-co","label":"partner-integration","scopes":["read"],"rate_limit_per_min":5000}`)
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
	if !strings.Contains(string(e.Metadata), `"target_account":"partner-co"`) {
		t.Errorf("audit metadata missing target account: %s", e.Metadata)
	}
	if !strings.Contains(string(e.Metadata), "acct:partner-co") {
		t.Errorf("audit metadata missing target identifier: %s", e.Metadata)
	}
}

// TestAdminKeysCreate_RateLimitDefaultsToCaller pins that an admin
// mint request that omits rate_limit_per_min must inherit the CALLER's
// own ceiling, not the deployment/tier default (0), which can exceed
// it. ClampToMinter's own-ceiling check only fires for
// rateLimitPerMin > 0, so a scoped operator on 500/min could otherwise
// mint an unrestricted (0 = default, potentially far higher) key by
// simply omitting the field. The clamped value must reach the store
// call, the response and the audit row alike.
func TestAdminKeysCreate_RateLimitDefaultsToCaller(t *testing.T) {
	store := &fakeAccountStore{
		rec:   auth.APIKeyRecord{KeyID: "kid_minted02"},
		plain: "sip_x",
	}
	sink := &recordingAuditSink{}
	caller := auth.Subject{
		Identifier:      "operator:staff-1",
		Tier:            auth.TierOperator,
		KeyID:           "kid_operator1",
		RateLimitPerMin: 500,
	}
	ts := newAdminTestServer(t, caller, store, sink)

	resp := postJSON(t, ts.URL+"/v1/admin/keys",
		`{"identifier":"acct:partner-co","account":"partner-co","label":"l"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if store.gotReq.RateLimitPerMin != 500 {
		t.Errorf("RateLimitPerMin = %d, want caller's own ceiling 500", store.gotReq.RateLimitPerMin)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(sink.entries))
	}
	if !strings.Contains(string(sink.entries[0].Metadata), `"rate_limit_per_min":500`) {
		t.Errorf("audit metadata rate_limit_per_min not clamped to 500: %s", sink.entries[0].Metadata)
	}
}

// TestAdminKeysCreate_RequiresReason pins the reason requirement.
// Minting a privileged credential is at least as consequential as
// setting a per-account override or killing a key, both of which hard-400
// without an X-Reason header. Without the header the audit row would
// record WHO minted WHAT, never WHY.
func TestAdminKeysCreate_RequiresReason(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_x"}, plain: "p"}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, operatorSubject(), store, sink)

	resp := postJSONWithReason(t, ts.URL+"/v1/admin/keys", "",
		`{"identifier":"acct:partner-co","account":"partner-co","label":"l"}`)
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
		`{"identifier":"acct:partner-co","account":"partner-co","label":"l"}`)
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
	resp := postJSON(t, ts.URL+"/v1/admin/keys", `{"identifier":"acct:x","account":"x","label":"l"}`)
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
		{"missing label", `{"identifier":"acct:x","account":"x"}`},
		{"bad tier", `{"identifier":"acct:x","account":"x","label":"l","tier":"sep10"}`},
		{"bad scope", `{"identifier":"acct:x","account":"x","label":"l","scopes":["everything"]}`},
		{"bad rate limit", `{"identifier":"acct:x","account":"x","label":"l","rate_limit_per_min":-1}`},
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

// TestAdminKeysRevoke_RevokesPostgresManagementRowToo is the
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

// TestAdminKeysCreate_InheritsTargetIdentifierCeiling — POST
// /v1/admin/keys takes no monthly_quota, so a key an operator mints for
// a metered customer's identifier must take the ceiling that
// identifier's credentials already carry, through the production store
// and validator. The audit row records the ceiling actually issued.
func TestAdminKeysCreate_InheritsTargetIdentifierCeiling(t *testing.T) {
	const planQuota int64 = 250_000
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := auth.NewRedisAPIKeyStore(rdb)
	if _, _, err := store.Create(context.Background(), auth.CreateAPIKeyRequest{
		Identifier: "acct:metered-co", Tier: auth.TierAPIKey, MonthlyQuota: planQuota,
	}); err != nil {
		t.Fatalf("seed metered key: %v", err)
	}

	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, operatorSubject(), store, sink)
	resp := postJSON(t, ts.URL+"/v1/admin/keys", `{"identifier":"acct:metered-co","account":"metered-co","label":"ops-minted"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/admin/keys status = %d, want 201", resp.StatusCode)
	}
	var body struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode mint response: %v", err)
	}
	sub, err := auth.NewRedisAPIKeyValidator(rdb).Lookup(context.Background(), body.Data.Plaintext)
	if err != nil {
		t.Fatalf("lookup minted key: %v", err)
	}
	if sub.MonthlyQuota != planQuota {
		t.Fatalf("operator-minted Subject.MonthlyQuota = %d, want %d (a zero cap bills the "+
			"customer's shared counter unmetered)", sub.MonthlyQuota, planQuota)
	}

	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(sink.entries))
	}
	var meta struct {
		MonthlyQuota *int64 `json:"monthly_quota"`
	}
	if err := json.Unmarshal(sink.entries[0].Metadata, &meta); err != nil {
		t.Fatalf("decode audit metadata: %v", err)
	}
	if meta.MonthlyQuota == nil || *meta.MonthlyQuota != planQuota {
		t.Fatalf("audit metadata monthly_quota = %v, want %d: %s",
			meta.MonthlyQuota, planQuota, sink.entries[0].Metadata)
	}
}

// An admin-minted key's identifier is its metering subject
// (middleware.UsageKeyForSubject), so an acct:<slug> identifier draws
// down that account's monthly quota. Such a mint must name the account
// explicitly and the account must exist; nothing is created otherwise.
func TestAdminKeysCreate_AccountBinding(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"acct identifier without confirmation", `{"identifier":"acct:partner-co","label":"l"}`, http.StatusBadRequest},
		{"confirmation names another account", `{"identifier":"acct:partner-co","account":"ok","label":"l"}`, http.StatusBadRequest},
		{"empty slug", `{"identifier":"acct:","account":"","label":"l"}`, http.StatusBadRequest},
		{"account on a non-account identifier", `{"identifier":"signup-abc","account":"partner-co","label":"l"}`, http.StatusBadRequest},
		{"account does not exist", `{"identifier":"acct:ghost","account":"ghost","label":"l"}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeAccountStore{}
			ts := newAdminTestServer(t, operatorSubject(), store, nil)
			resp := postJSON(t, ts.URL+"/v1/admin/keys", tc.body)
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if store.calls != 0 {
				t.Errorf("store.Create called %d times; a refused binding must mint nothing", store.calls)
			}
		})
	}
}

func TestAdminKeysCreate_NonAccountIdentifierNeedsNoBinding(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_ops"}, plain: "sip_x"}
	ts := newAdminTestServer(t, operatorSubject(), store, nil)
	resp := postJSON(t, ts.URL+"/v1/admin/keys", `{"identifier":"operator:staff-2","label":"l","tier":"operator"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if store.gotReq.Identifier != "operator:staff-2" {
		t.Errorf("Identifier = %q", store.gotReq.Identifier)
	}
}

// Without a platform account store the binding cannot be proven, so an
// acct:<slug> mint fails closed.
func TestAdminKeysCreate_AccountBindingFailsClosed(t *testing.T) {
	for name, accounts := range map[string]v1.PlatformAccountStore{
		"no account store": nil,
		"lookup error": &fakePlatformAccountStore{
			byID: map[uuid.UUID]platform.Account{}, getErr: errors.New("pg down"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeAccountStore{}
			srv := v1.New(v1.Options{
				Auth:             fakeAuthMiddleware(operatorSubject()),
				Accounts:         store,
				PlatformAccounts: accounts,
			})
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)
			resp := postJSON(t, ts.URL+"/v1/admin/keys", `{"identifier":"acct:partner-co","account":"partner-co","label":"l"}`)
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", resp.StatusCode)
			}
			if store.calls != 0 {
				t.Errorf("store.Create called %d times without a proven account", store.calls)
			}
		})
	}
}

// POST /v1/admin/keys must clamp scopes to the caller's but checked
// rate_limit_per_min only against the constant [0, 100000], so an
// operator key narrowed to "admin" could still mint 100,000/min keys.
func TestAdminKeysCreate_NarrowedOperatorCannotMintAboveItsRateLimit(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_minted"}, plain: "sip_x"}
	narrowed := auth.Subject{Identifier: "operator:staff-2", Tier: auth.TierOperator, KeyID: "kid_narrow", Scopes: []string{"admin"}}
	ts := newAdminTestServer(t, narrowed, store, nil)

	resp := postJSON(t, ts.URL+"/v1/admin/keys",
		`{"identifier":"acct:target","account":"target","label":"comp","rate_limit_per_min":100000}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a scoped operator on the default rate limit must not mint a 100000/min key", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("store.Create called %d times for a refused mint", store.calls)
	}
}

// The operator kill switch must not answer 204 and wrote a key.revoke
// audit row for a revoke that matched nothing, so an on-call engineer
// who mistyped one character of a leaked key's identifier was told the
// incident was contained while the key kept authenticating. Driven
// through the production store so the store's not-found contract and
// the handler's mapping of it are proven together.
func TestAdminKeysRevoke_TypoedIdentifierIs404WithNoAuditRow(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := auth.NewRedisAPIKeyStore(rdb)
	leaked, plaintext, err := store.Create(context.Background(),
		auth.CreateAPIKeyRequest{Identifier: "signup-9f3e", Label: "leaked"})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	sink := &recordingAuditSink{}
	ts := newAdminKeyServer(t, operatorSubject(), store, sink)

	resp := adminDelete(t, ts.URL+"/v1/admin/keys/"+leaked.KeyID+"?identifier=signup-9f3f", "key in a public gist")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — nothing was revoked, so the kill switch must not report success", resp.StatusCode)
	}
	for _, e := range sink.entries {
		if e.Action == "key.revoke" {
			t.Errorf("a key.revoke audit row was written for a revoke that revoked nothing: %+v", e)
		}
	}
	if _, err := auth.NewRedisAPIKeyValidator(rdb).Lookup(context.Background(), plaintext); err != nil {
		t.Fatalf("the untouched key should still authenticate (test premise): %v", err)
	}

	resp = adminDelete(t, ts.URL+"/v1/admin/keys/"+leaked.KeyID+"?identifier=signup-9f3e", "key in a public gist")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("correct identifier: status = %d, want 204", resp.StatusCode)
	}
	if len(sink.entries) != 1 || sink.entries[0].Action != "key.revoke" {
		t.Errorf("audit entries after the real revoke = %+v, want exactly one key.revoke", sink.entries)
	}
}

// A scoped caller asking for nothing must NOT get everything: the child
// inherits the parent's own scopes rather than defaulting to full
// access.
func TestAdminKeysCreate_NarrowedOperatorCannotMintUnscopedKey(t *testing.T) {
	before := testutil.ToFloat64(obs.MintScopeClampRefusedTotal.WithLabelValues("/v1/admin/keys"))

	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_minted01"}, plain: "sip_x"}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, narrowedOperatorSubject(), store, sink)

	resp := postJSON(t, ts.URL+"/v1/admin/keys",
		`{"identifier":"acct:self","account":"self","label":"escalation","tier":"operator","scopes":[]}`)
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
	// A narrowed request is not a refusal: the clamp silently narrows it
	// instead of rejecting, so the refusal counter must stay flat or the
	// alert built on it fires on every routine narrowing.
	if got := testutil.ToFloat64(obs.MintScopeClampRefusedTotal.WithLabelValues("/v1/admin/keys")); got != before {
		t.Errorf("mint_scope_clamp_refused_total{route=\"/v1/admin/keys\"} moved on a narrowed (not refused) mint: %v -> %v", before, got)
	}
}

// A scoped caller asking for a scope it does not hold is rejected
// outright — not silently narrowed, and never minted.
func TestAdminKeysCreate_NarrowedOperatorCannotMintScopeItLacks(t *testing.T) {
	before := testutil.ToFloat64(obs.MintScopeClampRefusedTotal.WithLabelValues("/v1/admin/keys"))

	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_minted02"}, plain: "sip_x"}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, narrowedOperatorSubject(), store, sink)

	resp := postJSON(t, ts.URL+"/v1/admin/keys",
		`{"identifier":"acct:partner-co","account":"partner-co","label":"data-reader","scopes":["read"]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — %q is outside the caller's own scopes", resp.StatusCode, platform.KeyScopeRead)
	}
	if store.calls != 0 {
		t.Fatalf("store.Create called %d times despite the scope refusal — the key was minted anyway", store.calls)
	}
	if len(sink.entries) != 0 {
		t.Fatalf("audit entries = %d, want 0 — nothing was minted", len(sink.entries))
	}
	// A refused escalation must be countable, not just logged —
	// a scope-narrowed key repeatedly probing for escalation otherwise
	// generates zero telemetry an alert could fire on.
	if got, want := testutil.ToFloat64(obs.MintScopeClampRefusedTotal.WithLabelValues("/v1/admin/keys")), before+1; got != want {
		t.Errorf("mint_scope_clamp_refused_total{route=\"/v1/admin/keys\"} = %v, want %v", got, want)
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
		`{"identifier":"acct:partner-co","account":"partner-co","label":"full","scopes":[]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if len(store.gotReq.Scopes) != 0 {
		t.Fatalf("minted Scopes = %v, want the empty (full-access) list an unscoped operator may still delegate",
			store.gotReq.Scopes)
	}
}

// TestAdminKeysRevoke_KillsALeakedKey is the key-half regression: an
// operator can revoke a credential belonging to somebody else. Without the kill switch
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
