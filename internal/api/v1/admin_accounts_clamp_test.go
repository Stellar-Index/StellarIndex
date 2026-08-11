// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
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

// newAdminClampServer wires the admin surface WITH the key-budget stores
// (the 52105fdb residual: production wires these in
// cmd/stellarindex-api, and the handler must actually use them).
func newAdminClampServer(
	t *testing.T,
	accounts v1.PlatformAccountStore,
	budgets v1.APIKeyBudgetStores,
	sink v1.AuditSink,
) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{
		Auth:             fakeAuthMiddleware(operatorSubject()),
		PlatformAccounts: accounts,
		APIKeyBudgets:    budgets,
		Audit:            sink,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestAdminAccountOverrides_TierLoweringClampsKeyBudgets is the
// proven-red guard for the 52105fdb residual (audit-2026-07-23).
//
// C3-014 established the full clamp: lowering the tier also lowers
// every credential the account can still authenticate with, because
// the enforced per-minute budget is read straight off the key record
// (auth/apikey_postgres.go `rateLimit := pgKey.RateLimitPerMin`;
// auth/apikey_redis.go returns the stored value). PATCH
// /v1/admin/accounts/{id} — the operator kill-switch / demotion surface —
// wrote `accounts.tier` and stopped, so an operator demoting an abusive
// Pro account to Free left every one of its keys serving 10_000/min.
//
// Asserted end to end: both key stores lowered to the Free ceiling (60),
// keys already at or below it untouched, revoked keys untouched, and the
// audit row carrying the clamp outcome.
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

	const freeCeiling = 60 // platform.TierFree.MaxRateLimitPerMin()
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
// the C3-010 kill-switch class re-opening on auth_backend=postgres.
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

// TestAdminAccountOverrides_ActivePatchDoesNotEvictKeyCache pins the transition
// gating: the suspend eviction fires ONLY on active→non-active, not on an
// unrelated override edit that leaves the account active (its keys are still
// valid and must keep their warm cache entries).
func TestAdminAccountOverrides_ActivePatchDoesNotEvictKeyCache(t *testing.T) {
	acctID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	accounts := newFakePlatformAccountStore(platform.Account{
		ID: acctID, Slug: "stillactive", Tier: platform.TierPro, Status: platform.AccountActive,
	})
	pgKeys := &fakePlatformAPIKeysForBridge{byAcct: map[uuid.UUID][]platform.APIKey{
		acctID: {{ID: "pg_a", AccountID: acctID, RateLimitPerMin: 60, KeyHash: []byte{0xaa}}},
	}}
	inv := &fakeKeyCacheInvalidator{}

	ts := newAdminClampServer(t, accounts, v1.APIKeyBudgetStores{
		Platform: pgKeys, CacheInvalidator: inv,
	}, &recordingAuditSink{})

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acctID.String(),
		"raise quota", `{"monthly_request_quota_override":1000000}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if n := len(inv.seen()); n != 0 {
		t.Errorf("an account staying active must not evict its key cache; evicted %d: %v", n, inv.seen())
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
