// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// fakePlatformAccountStore implements v1.PlatformAccountStore over an
// in-memory map so the handler tests exercise the wire shape without
// standing up Postgres.
type fakePlatformAccountStore struct {
	byID        map[uuid.UUID]platform.Account
	getErr      error
	updateErr   error
	updateCalls int
	lastUpdate  platform.Account
}

func newFakePlatformAccountStore(a platform.Account) *fakePlatformAccountStore {
	return &fakePlatformAccountStore{byID: map[uuid.UUID]platform.Account{a.ID: a}}
}

func (f *fakePlatformAccountStore) Get(_ context.Context, id uuid.UUID) (platform.Account, error) {
	if f.getErr != nil {
		return platform.Account{}, f.getErr
	}
	a, ok := f.byID[id]
	if !ok {
		return platform.Account{}, platform.ErrNotFound
	}
	return a, nil
}

func (f *fakePlatformAccountStore) GetBySlug(_ context.Context, slug string) (platform.Account, error) {
	if f.getErr != nil {
		return platform.Account{}, f.getErr
	}
	for _, a := range f.byID {
		if a.Slug == slug {
			return a, nil
		}
	}
	return platform.Account{}, platform.ErrNotFound
}

func (f *fakePlatformAccountStore) Update(_ context.Context, a platform.Account) error {
	f.updateCalls++
	f.lastUpdate = a
	if f.updateErr != nil {
		return f.updateErr
	}
	f.byID[a.ID] = a
	return nil
}

// UpdateAtomic is a sequential (non-locking) stand-in for
// postgresstore.AccountStore.UpdateAtomic: real concurrent-PATCH
// serialisation is only provable against Postgres's row lock (see
// TestAccountStoreUpdateAtomic_SerialisesConcurrentPatches), so these
// handler-level tests only need the same before/after + error routing
// the real implementation exposes.
func (f *fakePlatformAccountStore) UpdateAtomic(
	ctx context.Context, id uuid.UUID, mutate func(*platform.Account) error,
) (before, after platform.Account, err error) {
	before, err = f.Get(ctx, id)
	if err != nil {
		return platform.Account{}, platform.Account{}, err
	}
	after = before
	if err := mutate(&after); err != nil {
		return platform.Account{}, platform.Account{}, err
	}
	if err := f.Update(ctx, after); err != nil {
		return platform.Account{}, platform.Account{}, err
	}
	return before, after, nil
}

func newAdminAccountServer(t *testing.T, subject auth.Subject, store v1.PlatformAccountStore, sink v1.AuditSink) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{
		Auth:             fakeAuthMiddleware(subject),
		PlatformAccounts: store,
		Audit:            sink,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func patchJSON(t *testing.T, url, reason, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest PATCH %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if reason != "" {
		req.Header.Set("X-Reason", reason)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func seededAccount() platform.Account {
	return platform.Account{
		ID:     uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Name:   "Acme Corp",
		Slug:   "acme",
		Tier:   platform.TierStarter,
		Status: platform.AccountActive,
	}
}

// TestAdminAccountOverrides_Happy pins the operator tier-override path:
// the PATCH sets tier + both overrides, persists via Update, and lands
// one "account.override.set" audit row carrying the reason + before/after.
func TestAdminAccountOverrides_Happy(t *testing.T) {
	acct := seededAccount()
	store := newFakePlatformAccountStore(acct)
	sink := &recordingAuditSink{}
	ts := newAdminAccountServer(t, operatorSubject(), store, sink)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "enterprise comp",
		`{"tier":"enterprise","rate_limit_per_min_override":50000,"monthly_request_quota_override":200000000}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	if store.updateCalls != 1 {
		t.Fatalf("Update called %d times, want 1", store.updateCalls)
	}
	// Legacy "enterprise" in the PATCH body canonicalises to partner
	// in memory (free-platform model); the storage layer folds it back
	// to a CHECK-legal legacy string via Tier.StorageValue at write.
	if store.lastUpdate.Tier != platform.TierPartner {
		t.Errorf("persisted Tier = %q, want partner (canonical form of legacy enterprise)", store.lastUpdate.Tier)
	}
	if store.lastUpdate.RateLimitPerMinOverride != 50000 {
		t.Errorf("persisted RateLimitPerMinOverride = %d, want 50000", store.lastUpdate.RateLimitPerMinOverride)
	}
	if store.lastUpdate.MonthlyRequestQuotaOverride != 200000000 {
		t.Errorf("persisted MonthlyRequestQuotaOverride = %d", store.lastUpdate.MonthlyRequestQuotaOverride)
	}

	var env struct {
		Data v1.AdminAccountView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Tier != "partner" || env.Data.RateLimitPerMinOverride != 50000 {
		t.Errorf("response view = %+v", env.Data)
	}

	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(sink.entries))
	}
	e := sink.entries[0]
	if e.Action != "account.override.set" || e.ActorKind != platform.ActorStaff ||
		e.TargetKind != "account" || e.TargetID != acct.ID.String() {
		t.Errorf("audit entry = %+v", e)
	}
	if !strings.Contains(string(e.Metadata), "enterprise comp") {
		t.Errorf("audit metadata missing reason: %s", e.Metadata)
	}
}

// TestAdminAccountOverrides_ClearOverride pins that override=0 clears
// (round-trips as inherit-tier-default: the store NULLIFs it).
func TestAdminAccountOverrides_ClearOverride(t *testing.T) {
	acct := seededAccount()
	acct.RateLimitPerMinOverride = 12345
	store := newFakePlatformAccountStore(acct)
	ts := newAdminAccountServer(t, operatorSubject(), store, nil)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "reset",
		`{"rate_limit_per_min_override":0}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if store.lastUpdate.RateLimitPerMinOverride != 0 {
		t.Errorf("override not cleared: %d", store.lastUpdate.RateLimitPerMinOverride)
	}
}

func TestAdminAccountOverrides_MissingReason400(t *testing.T) {
	acct := seededAccount()
	store := newFakePlatformAccountStore(acct)
	ts := newAdminAccountServer(t, operatorSubject(), store, nil)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "",
		`{"tier":"pro"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 without X-Reason", resp.StatusCode)
	}
	if store.updateCalls != 0 {
		t.Errorf("Update called despite missing reason")
	}
}

func TestAdminAccountOverrides_NonOperator403(t *testing.T) {
	acct := seededAccount()
	store := newFakePlatformAccountStore(acct)
	ts := newAdminAccountServer(t, auth.Subject{
		Identifier: "acct:customer", Tier: auth.TierAPIKey, KeyID: "kid_cust",
	}, store, nil)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "nope",
		`{"tier":"enterprise"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for non-operator", resp.StatusCode)
	}
	if store.updateCalls != 0 {
		t.Errorf("Update called by a non-operator")
	}
}

func TestAdminAccountOverrides_Anonymous401(t *testing.T) {
	acct := seededAccount()
	ts := newAdminAccountServer(t, auth.Subject{}, newFakePlatformAccountStore(acct), nil)
	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "x", `{"tier":"pro"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAdminAccountOverrides_NotFound404(t *testing.T) {
	acct := seededAccount()
	store := newFakePlatformAccountStore(acct)
	ts := newAdminAccountServer(t, operatorSubject(), store, nil)

	other := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+other.String(), "x", `{"tier":"pro"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for absent account", resp.StatusCode)
	}
}

func TestAdminAccountOverrides_Validation(t *testing.T) {
	acct := seededAccount()
	cases := []struct {
		name string
		body string
	}{
		{"empty patch", `{}`},
		{"bad tier", `{"tier":"platinum"}`},
		{"negative rate limit", `{"rate_limit_per_min_override":-5}`},
		{"rate limit too high", `{"rate_limit_per_min_override":200000}`},
		{"negative monthly quota", `{"monthly_request_quota_override":-1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakePlatformAccountStore(acct)
			ts := newAdminAccountServer(t, operatorSubject(), store, nil)
			resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "x", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			if store.updateCalls != 0 {
				t.Errorf("Update called on invalid input")
			}
		})
	}
}

func TestAdminAccountOverrides_StoreUnwired503(t *testing.T) {
	// No PlatformAccounts wired → 503.
	srv := v1.New(v1.Options{Auth: fakeAuthMiddleware(operatorSubject())})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+seededAccount().ID.String(), "x", `{"tier":"pro"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when store unwired", resp.StatusCode)
	}
}

func TestAdminAccountGet_Happy(t *testing.T) {
	acct := seededAccount()
	acct.RateLimitPerMinOverride = 9000
	store := newFakePlatformAccountStore(acct)
	ts := newAdminAccountServer(t, operatorSubject(), store, nil)

	resp, err := http.Get(ts.URL + "/v1/admin/accounts/" + acct.ID.String())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.AdminAccountView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Slug != "acme" || env.Data.RateLimitPerMinOverride != 9000 {
		t.Errorf("view = %+v", env.Data)
	}
	// The operator surface must resolve the override the same
	// way platform.Account's cascade does (free-tier ceiling 1000,
	// raised to the 9000 override), not merely echo it back raw.
	if env.Data.EffectiveRateLimitPerMin != 9000 {
		t.Errorf("EffectiveRateLimitPerMin = %d, want 9000", env.Data.EffectiveRateLimitPerMin)
	}
	if env.Data.EffectiveMonthlyQuota != 1_000_000 {
		t.Errorf("EffectiveMonthlyQuota = %d, want 1000000 (free-tier ceiling; no quota override set)", env.Data.EffectiveMonthlyQuota)
	}
}

// TestAdminAccountOverrides_CloseRevokesKeys pins that closing an account
// revokes every live key (and evicts it from the auth cache) instead of
// leaving closure as a status string the credentials outlive.
func TestAdminAccountOverrides_CloseRevokesKeys(t *testing.T) {
	acct := seededAccount()
	accounts := newFakePlatformAccountStore(acct)
	pgKeys := &fakePlatformAPIKeysForBridge{byAcct: map[uuid.UUID][]platform.APIKey{
		acct.ID: {
			{ID: "pg_a", AccountID: acct.ID, KeyHash: []byte{0xaa}},
			{ID: "pg_b", AccountID: acct.ID, KeyHash: []byte{0xbb}},
		},
	}}
	inv := &fakeKeyCacheInvalidator{}
	sink := &recordingAuditSink{}
	ts := newAdminClampServer(t, accounts, v1.APIKeyBudgetStores{Platform: pgKeys, CacheInvalidator: inv}, sink)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "customer asked to close", `{"status":"closed"}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if got := pgKeys.revokedIDs(); strings.Join(got, ",") != "pg_a,pg_b" {
		t.Fatalf("revoked keys = %v, want [pg_a pg_b]: a closed account's keys must not outlive it", got)
	}
	for _, k := range pgKeys.byAcct[acct.ID] {
		if k.RevokedReason != "account closed" {
			t.Errorf("key %s revoked_reason = %q, want %q", k.ID, k.RevokedReason, "account closed")
		}
	}
	if got := strings.Join(inv.seen(), ","); !strings.Contains(got, "aa") || !strings.Contains(got, "bb") {
		t.Errorf("cache evictions = %q, want both revoked keys evicted", got)
	}
	if len(sink.entries) != 1 || !strings.Contains(string(sink.entries[0].Metadata), `"keys_revoked":2`) {
		t.Errorf("audit row must record keys_revoked=2; entries=%+v", sink.entries)
	}
}

// TestAdminAccountOverrides_ClosedIsTerminal pins that a closed account
// cannot be moved back to active or suspended, so no status edit resurrects
// the passkeys, sessions and webhooks closure left in place.
func TestAdminAccountOverrides_ClosedIsTerminal(t *testing.T) {
	for _, target := range []string{"active", "suspended"} {
		t.Run(target, func(t *testing.T) {
			acct := seededAccount()
			acct.Status = platform.AccountClosed
			store := newFakePlatformAccountStore(acct)
			ts := newAdminAccountServer(t, operatorSubject(), store, &recordingAuditSink{})

			resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "reopen", `{"status":"`+target+`"}`)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("closed→%s status = %d, want 409", target, resp.StatusCode)
			}
			if store.updateCalls != 0 || store.byID[acct.ID].Status != platform.AccountClosed {
				t.Errorf("closed account was rewritten: updates=%d status=%q", store.updateCalls, store.byID[acct.ID].Status)
			}
		})
	}
	t.Run("closed stays patchable", func(t *testing.T) {
		acct := seededAccount()
		acct.Status = platform.AccountClosed
		store := newFakePlatformAccountStore(acct)
		ts := newAdminAccountServer(t, operatorSubject(), store, &recordingAuditSink{})
		resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "retry", `{"status":"closed"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("closed→closed status = %d, want 200 (the retry path for a failed revoke)", resp.StatusCode)
		}
	})
}

// fakeAccountSessionRevoker records which members had their sessions revoked.
type fakeAccountSessionRevoker struct {
	members   map[uuid.UUID][]platform.User
	revokeErr map[uuid.UUID]error
	revoked   []uuid.UUID
}

func (f *fakeAccountSessionRevoker) ListUsersForAccount(_ context.Context, accountID uuid.UUID) ([]platform.User, error) {
	return f.members[accountID], nil
}

func (f *fakeAccountSessionRevoker) RevokeAllUserSessions(_ context.Context, userID uuid.UUID) error {
	if err := f.revokeErr[userID]; err != nil {
		return err
	}
	f.revoked = append(f.revoked, userID)
	return nil
}

// TestAdminAccountOverrides_CloseRevokesSessions pins that closing an
// account revokes every member's dashboard sessions and records the outcome
// in the audit row; a suspension leaves sessions to the status gate.
func TestAdminAccountOverrides_CloseRevokesSessions(t *testing.T) {
	acct := seededAccount()
	alice := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	bob := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	newServer := func(store v1.PlatformAccountStore, users *fakeAccountSessionRevoker, sink v1.AuditSink) *httptest.Server {
		srv := v1.New(v1.Options{
			Auth:             fakeAuthMiddleware(operatorSubject()),
			PlatformAccounts: store,
			PlatformUsers:    users,
			Audit:            sink,
		})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		return ts
	}
	members := map[uuid.UUID][]platform.User{acct.ID: {{ID: alice}, {ID: bob}}}

	t.Run("closed", func(t *testing.T) {
		users := &fakeAccountSessionRevoker{members: members, revokeErr: map[uuid.UUID]error{bob: io.ErrUnexpectedEOF}}
		sink := &recordingAuditSink{}
		ts := newServer(newFakePlatformAccountStore(acct), users, sink)
		resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "customer asked to close", `{"status":"closed"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if len(users.revoked) != 1 || users.revoked[0] != alice {
			t.Fatalf("revoked sessions for %v, want [alice]: a closed account's sessions must not outlive it", users.revoked)
		}
		meta := string(sink.entries[0].Metadata)
		if !strings.Contains(meta, `"session_users_revoked":1`) || !strings.Contains(meta, `"session_revoke_failures":1`) {
			t.Errorf("audit row must record the session sweep outcome; metadata=%s", meta)
		}
	})
	t.Run("suspended", func(t *testing.T) {
		users := &fakeAccountSessionRevoker{members: members}
		ts := newServer(newFakePlatformAccountStore(acct), users, &recordingAuditSink{})
		resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "abuse", `{"status":"suspended"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if len(users.revoked) != 0 {
			t.Errorf("suspension revoked sessions %v; only terminal closure should", users.revoked)
		}
	})
}

// TestAdminAccountGet_AppendsReadAudit pins CA2-A39-harden-0: the operator
// account read returns the billing email, so each successful GET lands one
// durable "admin.account.read" row naming the operator credential and the
// account it read, and a refused read lands none.
func TestAdminAccountGet_AppendsReadAudit(t *testing.T) {
	acct := seededAccount()
	acct.BillingEmail = "billing@acme.example"
	sink := &recordingAuditSink{}
	ts := newAdminAccountServer(t, operatorSubject(), newFakePlatformAccountStore(acct), sink)

	resp, err := http.Get(ts.URL + "/v1/admin/accounts/" + acct.ID.String())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1: an operator read of billing PII must be recorded", len(sink.entries))
	}
	e := sink.entries[0]
	if e.Action != "admin.account.read" || e.TargetKind != "account" || e.TargetID != acct.ID.String() ||
		e.AccountID != acct.ID || e.ActorKind != platform.ActorStaff {
		t.Errorf("audit entry = %+v, want admin.account.read on account %s by staff", e, acct.ID)
	}
	var meta map[string]any
	if err := json.Unmarshal(e.Metadata, &meta); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta["actor_key_id"] != operatorSubject().KeyID {
		t.Errorf("metadata actor_key_id = %v, want %q", meta["actor_key_id"], operatorSubject().KeyID)
	}
	if _, ok := meta["account_slug"]; ok {
		t.Errorf("metadata carries account_slug; target_id already names the account")
	}

	missing, err := http.Get(ts.URL + "/v1/admin/accounts/" + uuid.New().String())
	if err != nil {
		t.Fatalf("GET missing: %v", err)
	}
	t.Cleanup(func() { missing.Body.Close() })
	if missing.StatusCode != http.StatusNotFound || len(sink.entries) != 1 {
		t.Errorf("404 read: status=%d entries=%d, want 404 and no new row", missing.StatusCode, len(sink.entries))
	}
}

// TestAdminAccountOverrides_TierLoweringClampsKeyBudgets is the
// proven-red guard for the clamp.
//
// The full clamp: lowering the tier also lowers
// every credential the account can still authenticate with, because
// the enforced per-minute budget is read straight off the key record
// (auth/apikey_postgres.go `rateLimit := pgKey.RateLimitPerMin`;
// auth/apikey_redis.go returns the stored value). PATCH
// /v1/admin/accounts/{id} — the operator kill-switch / demotion surface —
// wrote `accounts.tier` and stopped, so an operator demoting an abusive
// Pro account to Free left every one of its keys serving 10_000/min.
//
// Asserted end to end: both key stores lowered to the free ceiling
// (1000/min under the free-platform model — legacy "pro" canonicalises
// to partner, 100k ceiling), keys already at or below it untouched,
// revoked keys untouched, and the audit row carrying the clamp outcome.
func TestAdminAccountOverrides_TierLoweringClampsKeyBudgets(t *testing.T) {
	acctID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	acct := platform.Account{
		ID:     acctID,
		Name:   "Abusive Corp",
		Slug:   "abusive",
		Tier:   platform.TierPro,
		Status: platform.AccountActive,
	}
	accounts := newFakePlatformAccountStore(acct)

	pgKeys := &fakePlatformAPIKeysForBridge{byAcct: map[uuid.UUID][]platform.APIKey{
		acctID: {
			{ID: "pg_hot", AccountID: acctID, RateLimitPerMin: 10_000, KeyHash: []byte{0xaa}},
			{ID: "pg_already_low", AccountID: acctID, RateLimitPerMin: 30},
		},
	}}
	abusiveID := auth.AccountIdentifier("abusive")
	redisKeys := &fakeSelfServiceKeyManager{keys: map[string][]auth.APIKeyRecord{
		abusiveID: {
			{KeyID: "rk_hot", Identifier: abusiveID, RateLimitPerMin: 10_000},
			{KeyID: "rk_low", Identifier: abusiveID, RateLimitPerMin: 60},
		},
	}}
	sink := &recordingAuditSink{}

	ts := newAdminClampServer(t, accounts, v1.APIKeyBudgetStores{
		Platform: pgKeys,
		Redis:    redisKeys,
	}, sink)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acctID.String(),
		"abuse: demote to free", `{"tier":"free"}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	const freeCeiling = 1000 // platform.TierFree.MaxRateLimitPerMin()
	if got := platform.TierFree.MaxRateLimitPerMin(); got != freeCeiling {
		t.Fatalf("tier ladder moved: TierFree.MaxRateLimitPerMin() = %d, want %d", got, freeCeiling)
	}

	// ── Postgres-backed dashboard keys ──────────────────────────────
	if len(pgKeys.updates) != 1 {
		t.Fatalf("platform key Update called %d time(s), want exactly 1 "+
			"(only the over-ceiling key); updates=%+v", len(pgKeys.updates), pgKeys.updates)
	}
	if pgKeys.updates[0].ID != "pg_hot" {
		t.Errorf("lowered platform key = %q, want pg_hot", pgKeys.updates[0].ID)
	}
	if got := pgKeys.updates[0].RateLimitPerMin; got != freeCeiling {
		t.Errorf("platform key rate_limit_per_min = %d, want %d — the Free tier's ceiling, "+
			"not the Pro budget the account no longer pays for", got, freeCeiling)
	}

	// ── Redis-backed self-service keys ──────────────────────────────
	if len(redisKeys.updates) != 1 {
		t.Fatalf("redis UpdateRateLimit called %d time(s), want exactly 1; updates=%+v",
			len(redisKeys.updates), redisKeys.updates)
	}
	if redisKeys.updates[0].keyID != "rk_hot" {
		t.Errorf("lowered redis key = %q, want rk_hot", redisKeys.updates[0].keyID)
	}
	if got := redisKeys.updates[0].rateLimit; got != freeCeiling {
		t.Errorf("redis key rate_limit_per_min = %d, want %d", got, freeCeiling)
	}

	// ── the durable trace ───────────────────────────────────────────
	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(sink.entries))
	}
	meta := string(sink.entries[0].Metadata)
	if !strings.Contains(meta, `"keys_clamped":2`) {
		t.Errorf("audit metadata missing keys_clamped:2 (the operator-visible clamp outcome): %s", meta)
	}
	if !strings.Contains(meta, `"key_clamp_failures":0`) {
		t.Errorf("audit metadata missing key_clamp_failures:0: %s", meta)
	}
}

// TestAdminAccountOverrides_TierRaiseDoesNotTouchKeys — the clamp is
// one-directional on purpose. Budgets are raised at MINT time by the
// dashboard's clampRateLimitToTier; silently lifting existing keys on a
// comp would grant throughput the operator never asked for.
func TestAdminAccountOverrides_TierRaiseDoesNotTouchKeys(t *testing.T) {
	acctID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	accounts := newFakePlatformAccountStore(platform.Account{
		ID: acctID, Slug: "comped", Tier: platform.TierFree, Status: platform.AccountActive,
	})
	pgKeys := &fakePlatformAPIKeysForBridge{byAcct: map[uuid.UUID][]platform.APIKey{
		acctID: {{ID: "pg_k", AccountID: acctID, RateLimitPerMin: 60}},
	}}
	redisKeys := &fakeSelfServiceKeyManager{keys: map[string][]auth.APIKeyRecord{
		auth.AccountIdentifier("comped"): {{KeyID: "rk_k", RateLimitPerMin: 60}},
	}}

	ts := newAdminClampServer(t, accounts, v1.APIKeyBudgetStores{
		Platform: pgKeys, Redis: redisKeys,
	}, &recordingAuditSink{})

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acctID.String(),
		"comp to pro", `{"tier":"pro"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(pgKeys.updates) != 0 || len(redisKeys.updates) != 0 {
		t.Errorf("a tier RAISE must not rewrite key budgets; platform=%+v redis=%+v",
			pgKeys.updates, redisKeys.updates)
	}
}

// TestAdminAccountOverrides_SuspendEvictsKeyCache is the proven-red guard for
// the kill-switch class re-opening on auth_backend=postgres.
//
// The Postgres validator's cache-HIT path (auth/apikey_postgres.go
// cacheLookup) checks only the KEY's revoked/expired fields, never the account
// status, so persisting `accounts.status='suspended'` left every already-cached
// key authenticating for up to the ~1h read-through TTL. The tier clamp does
// not help — it early-returns unless the tier ceiling drops, so a pure status
// flip evicts nothing. handleAdminAccountOverrides now evicts every cached key
// for an account that transitions active→non-active, reusing the tier-clamp
// seam (ListForAccount + InvalidateCachedKey), so the next request misses the
// cache, re-reads Postgres, and is refused by the existing cache-miss
// `acct.Status != AccountActive` gate.
//
// Prove-red: without the eviction wiring, InvalidateCachedKey is never called
// and both keys stay warm — a suspended account keeps authenticating.
func TestAdminAccountOverrides_SuspendEvictsKeyCache(t *testing.T) {
	acctID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	acct := platform.Account{
		ID:     acctID,
		Name:   "Kill Switch Corp",
		Slug:   "killswitch",
		Tier:   platform.TierPro,
		Status: platform.AccountActive,
	}
	accounts := newFakePlatformAccountStore(acct)

	pgKeys := &fakePlatformAPIKeysForBridge{byAcct: map[uuid.UUID][]platform.APIKey{
		acctID: {
			{ID: "pg_a", AccountID: acctID, RateLimitPerMin: 60, KeyHash: []byte{0xaa}},
			{ID: "pg_b", AccountID: acctID, RateLimitPerMin: 60, KeyHash: []byte{0xbb}},
		},
	}}
	inv := &fakeKeyCacheInvalidator{}
	sink := &recordingAuditSink{}

	ts := newAdminClampServer(t, accounts, v1.APIKeyBudgetStores{
		Platform:         pgKeys,
		CacheInvalidator: inv,
	}, sink)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acctID.String(),
		"abuse: kill switch", `{"status":"suspended"}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	// The tier is unchanged (Pro→Pro), so the clamp is a no-op; every eviction
	// here is the suspend path's doing, not the clamp's.
	if len(pgKeys.updates) != 0 {
		t.Errorf("a status-only PATCH must not rewrite key budgets; updates=%+v", pgKeys.updates)
	}

	// Every cached key for the now-suspended account must have been evicted so
	// the validator's cache-hit path can't keep authenticating it past the TTL.
	got := inv.seen()
	want := map[string]bool{"aa": false, "bb": false}
	for _, h := range got {
		if _, ok := want[h]; !ok {
			t.Errorf("unexpected cache eviction for hash %q", h)
			continue
		}
		want[h] = true
	}
	for h, evicted := range want {
		if !evicted {
			t.Errorf("cached key %q was NOT evicted on suspend; a suspended account keeps "+
				"authenticating for up to the validator TTL", h)
		}
	}
}

// TestAdminAccountOverrides_EnforcementNeutralPatchDoesNotEvictKeyCache pins
// that the eviction is GATED on an enforcement-relevant change, not fired on
// every PATCH. A tier RAISE that leaves status active and touches neither
// override changes nothing the Postgres validator resolves onto a cached key
// (the resolved Subject's tier comes from the KEY row, and the clamp only fires
// on a ceiling DROP), so its warm cache entries must be left intact.
func TestAdminAccountOverrides_EnforcementNeutralPatchDoesNotEvictKeyCache(t *testing.T) {
	acctID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	accounts := newFakePlatformAccountStore(platform.Account{
		ID: acctID, Slug: "stillactive", Tier: platform.TierFree, Status: platform.AccountActive,
	})
	pgKeys := &fakePlatformAPIKeysForBridge{byAcct: map[uuid.UUID][]platform.APIKey{
		acctID: {{ID: "pg_a", AccountID: acctID, RateLimitPerMin: 60, KeyHash: []byte{0xaa}}},
	}}
	inv := &fakeKeyCacheInvalidator{}

	ts := newAdminClampServer(t, accounts, v1.APIKeyBudgetStores{
		Platform: pgKeys, CacheInvalidator: inv,
	}, &recordingAuditSink{})

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acctID.String(),
		"comp to pro", `{"tier":"pro"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if n := len(inv.seen()); n != 0 {
		t.Errorf("an enforcement-neutral PATCH must not evict its key cache; evicted %d: %v", n, inv.seen())
	}
}

// TestAdminAccountOverrides_OverrideChangeEvictsKeyCache is the proven-red guard
// for the rule that an admin override-only PATCH must evict the
// account's warm key-cache entries so the tightened ceiling is enforced now,
// not after the Postgres validator's ~1h read-through TTL.
//
// The validator RESOLVES a key's effective monthly-quota ceiling and per-minute
// floor from the account overrides at Lookup time (auth/apikey_postgres.go) and
// caches the resolved Subject verbatim. The tier-clamp seam (early-returns
// unless the ceiling drops) and the suspend seam (early-returns unless status
// leaves active) both skip an override-only change, so without explicit
// invalidation every warm key keeps authenticating with the OLD unmetered/higher
// quota until the TTL rolls it off — the just-tightened operator ceiling
// silently unenforced.
//
// Prove-red: without the override eviction wiring, InvalidateCachedKey is never
// called for a status-unchanged override PATCH and both keys stay warm.
func TestAdminAccountOverrides_OverrideChangeEvictsKeyCache(t *testing.T) {
	acctID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	acct := platform.Account{
		ID:                          acctID,
		Name:                        "Metered Abuser Corp",
		Slug:                        "metered",
		Tier:                        platform.TierPartner,
		Status:                      platform.AccountActive,
		MonthlyRequestQuotaOverride: 0, // currently unmetered
	}
	accounts := newFakePlatformAccountStore(acct)

	pgKeys := &fakePlatformAPIKeysForBridge{byAcct: map[uuid.UUID][]platform.APIKey{
		acctID: {
			{ID: "pg_a", AccountID: acctID, RateLimitPerMin: 60, KeyHash: []byte{0xaa}},
			{ID: "pg_b", AccountID: acctID, RateLimitPerMin: 60, KeyHash: []byte{0xbb}},
		},
	}}
	inv := &fakeKeyCacheInvalidator{}

	ts := newAdminClampServer(t, accounts, v1.APIKeyBudgetStores{
		Platform:         pgKeys,
		CacheInvalidator: inv,
	}, &recordingAuditSink{})

	// Tighten the monthly-quota ceiling from unmetered (0) to 1000, leaving tier
	// and status untouched — the exact override-only PATCH that must evict the key cache.
	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acctID.String(),
		"abuse: cap metered volume", `{"monthly_request_quota_override":1000}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	// Tier is unchanged (partner→partner) and status stays active, so neither the
	// clamp nor the suspend seam fires; the persisted override must not rewrite
	// any key budget either.
	if len(pgKeys.updates) != 0 {
		t.Errorf("an override-only PATCH must not rewrite key budgets; updates=%+v", pgKeys.updates)
	}

	got := inv.seen()
	want := map[string]bool{"aa": false, "bb": false}
	for _, h := range got {
		if _, ok := want[h]; !ok {
			t.Errorf("unexpected cache eviction for hash %q", h)
			continue
		}
		want[h] = true
	}
	for h, evicted := range want {
		if !evicted {
			t.Errorf("cached key %q was NOT evicted on an override tightening; the account keeps "+
				"authenticating at the OLD ceiling for up to the validator TTL", h)
		}
	}
}

// TestAdminAccountOverrides_NoKeyStoresWiredStillPatches — a deployment
// with no key store degrades visibly (keys_clamped=0) rather than
// 500-ing or silently pretending the clamp happened.
func TestAdminAccountOverrides_NoKeyStoresWiredStillPatches(t *testing.T) {
	acct := seededAccount()
	accounts := newFakePlatformAccountStore(acct)
	sink := &recordingAuditSink{}
	ts := newAdminClampServer(t, accounts, v1.APIKeyBudgetStores{}, sink)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(),
		"demote", `{"tier":"free"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if accounts.lastUpdate.Tier != platform.TierFree {
		t.Errorf("persisted Tier = %q, want free", accounts.lastUpdate.Tier)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(sink.entries))
	}
	if meta := string(sink.entries[0].Metadata); !strings.Contains(meta, `"keys_clamped":0`) {
		t.Errorf("audit metadata should record keys_clamped:0 for an unwired deployment: %s", meta)
	}
}
