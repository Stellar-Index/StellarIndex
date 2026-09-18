// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// registerStyleFixture mints the credential pair POST /v1/register
// leaves behind: a Postgres management row carrying sha256(plaintext),
// and — through the production mirror seam — the Redis record keyed by
// the same hash. The plaintext is assembled at runtime from a repeated
// two-byte pattern so it is unmistakably a fixture.
func registerStyleFixture(
	t *testing.T, budgets v1.APIKeyBudgetStores, acct platform.Account,
) (plaintext, redisKey string, row platform.APIKey) {
	t.Helper()
	plaintext = "sip_" + strings.Repeat("ab", 32)
	sum := sha256.Sum256([]byte(plaintext))
	row = platform.APIKey{
		ID:              "kid_fixture_register",
		AccountID:       acct.ID,
		KeyHash:         sum[:],
		RateLimitPerMin: platform.TierFree.MaxRateLimitPerMin(),
	}
	if budgets.RedisMirror != nil {
		if err := budgets.RedisMirror.CreateWithSecret(context.Background(), auth.MirroredKey{
			Plaintext:       plaintext,
			KeyID:           row.ID,
			Identifier:      auth.AccountIdentifier(acct.Slug),
			Label:           "registration key",
			RateLimitPerMin: row.RateLimitPerMin,
		}); err != nil {
			t.Fatalf("mirror fixture credential: %v", err)
		}
	}
	return plaintext, cachekeys.APIKey(hex.EncodeToString(sum[:])).String(), row
}

// TestNewAPIKeyBudgetStores_RedisBackendNeverDeletesTheCredential pins
// findings F056 / K050 / Q145 at the handler seam.
//
// Under the default auth_backend=redis, `apikey:<hash>` IS the
// credential. The stores used to carry a key-cache invalidator whenever
// Redis was configured, so an override-only PATCH — a quota RAISE —
// DELeted every /v1/register key the account held, permanently.
func TestNewAPIKeyBudgetStores_RedisBackendNeverDeletesTheCredential(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	acct := platform.Account{
		ID:     uuid.MustParse("77777777-7777-7777-7777-777777777777"),
		Name:   "Registered Agent",
		Slug:   "reg-agent",
		Tier:   platform.TierFree,
		Status: platform.AccountActive,
	}
	pgKeys := &fakePlatformAPIKeysForBridge{byAcct: map[uuid.UUID][]platform.APIKey{}}
	budgets := v1.NewAPIKeyBudgetStores(pgKeys, rdb, "redis")

	if budgets.CacheInvalidator != nil {
		t.Errorf("CacheInvalidator = %#v under auth_backend=redis, want a nil interface: there is "+
			"no key cache in this mode, only the canonical credential", budgets.CacheInvalidator)
	}
	if budgets.Redis == nil || budgets.RedisMirror == nil {
		t.Fatal("the Redis self-service store and the register mirror must stay wired under auth_backend=redis")
	}

	plaintext, redisKey, row := registerStyleFixture(t, budgets, acct)
	pgKeys.byAcct[acct.ID] = []platform.APIKey{row}

	ts := newAdminClampServer(t, newFakePlatformAccountStore(acct), budgets, &recordingAuditSink{})
	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(),
		"promote to partner limits", `{"rate_limit_per_min_override":20000}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	if !mr.Exists(redisKey) {
		t.Fatalf("the PATCH deleted %s — the customer's only credential under auth_backend=redis", redisKey)
	}
	sub, err := auth.NewRedisAPIKeyValidator(rdb).Lookup(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("register-minted key no longer authenticates after an override raise: %v", err)
	}
	if sub.KeyID != row.ID {
		t.Errorf("authenticated as %q, want %q", sub.KeyID, row.ID)
	}
}

// TestNewAPIKeyBudgetStores_PostgresBackendStillEvicts is the other
// direction: the fix must not turn the eviction off where it is
// correct. Under auth_backend=postgres the same key is a rebuildable
// read-through cache entry, and leaving it in place serves the stale
// resolved budget for the validator TTL (F-A, audit-2026-08-14).
func TestNewAPIKeyBudgetStores_PostgresBackendStillEvicts(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	acct := platform.Account{
		ID:     uuid.MustParse("88888888-8888-8888-8888-888888888888"),
		Name:   "Dashboard Customer",
		Slug:   "dash-cust",
		Tier:   platform.TierPartner,
		Status: platform.AccountActive,
	}
	pgKeys := &fakePlatformAPIKeysForBridge{byAcct: map[uuid.UUID][]platform.APIKey{}}
	budgets := v1.NewAPIKeyBudgetStores(pgKeys, rdb, auth.BackendPostgres)
	if budgets.CacheInvalidator == nil {
		t.Fatal("CacheInvalidator is nil under auth_backend=postgres; override changes would " +
			"go unenforced until the validator cache TTL")
	}

	_, redisKey, row := registerStyleFixture(t, budgets, acct)
	pgKeys.byAcct[acct.ID] = []platform.APIKey{row}
	if !mr.Exists(redisKey) {
		t.Fatal("test setup: cache entry was not seeded")
	}

	ts := newAdminClampServer(t, newFakePlatformAccountStore(acct), budgets, &recordingAuditSink{})
	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(),
		"abuse: cap metered volume", `{"monthly_request_quota_override":1000}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if mr.Exists(redisKey) {
		t.Fatalf("cache entry %s survived an override tightening under auth_backend=postgres", redisKey)
	}
}

// TestNewAPIKeyBudgetStores_NilHalves — a deployment missing a store
// gets nil INTERFACES for that half, so the `== nil` guards at the
// clamp / eviction / register call sites still see "not wired".
func TestNewAPIKeyBudgetStores_NilHalves(t *testing.T) {
	for _, backend := range []string{"", "redis", auth.BackendPostgres} {
		st := v1.NewAPIKeyBudgetStores(nil, nil, backend)
		if st.Platform != nil || st.Redis != nil || st.RedisMirror != nil || st.CacheInvalidator != nil {
			t.Errorf("backend %q with no stores: got %+v, want every half nil", backend, st)
		}
	}
}
