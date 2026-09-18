//go:build integration

// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package integration_test

// Admin account PATCH vs. the register-minted credential, on the REAL
// stores (findings F056 / K050 / Q145).
//
// Under the default `auth_backend=redis` the record at `apikey:<hash>`
// is the canonical credential: RedisAPIKeyValidator reads nothing else,
// and a POST /v1/register key exists there only as the mirror written
// by RedisAPIKeyStore.CreateWithSecret. The "key cache invalidator" was
// wired whenever Redis was configured, so PATCH /v1/admin/accounts/{id}
// — on ANY override change, including the quota RAISE the register doc
// itself advertises — DELeted that record. The plaintext is shown once
// and Postgres keeps only its hash, so the customer's key was gone for
// good: a permanent 401 with no recovery path.
//
// This drives the whole scenario through the production seams rather
// than fakes: real Postgres account + api_keys rows, a real Redis
// holding the mirror, the real POST /v1/register and PATCH handlers,
// the real RedisAPIKeyValidator, and the stores assembled by
// v1.NewAPIKeyBudgetStores — the constructor cmd/stellarindex-api calls.
//
// Why real Redis and not miniredis: the property is "the record is
// still there, with its idle TTL, and still authenticates" after a
// sequence of server-side writes; the TTL half is only trustworthy
// against a server that actually tracks expiry.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// operatorHeader marks a request as coming from an operator. The test
// stands in for the auth middleware only — everything downstream of the
// resolved Subject is production code.
const operatorHeader = "X-Test-Operator"

func operatorWhenFlagged() middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(operatorHeader) != "" {
				r = r.WithContext(auth.WithSubject(r.Context(), auth.Subject{
					Identifier: "operator:staff-1",
					Tier:       auth.TierOperator,
					KeyID:      "kid_operator1",
				}))
			}
			next.ServeHTTP(w, r)
		})
	}
}

type adminPatchEnv struct {
	ts        *httptest.Server
	rdb       *redis.Client
	validator *auth.RedisAPIKeyValidator
	accounts  *postgresstore.AccountStore
	keys      platform.APIKeyStore
}

func newAdminPatchEnv(t *testing.T, db *sql.DB, rdb *redis.Client, authBackend string) adminPatchEnv {
	t.Helper()
	pg := postgresstore.New(db)
	accounts := postgresstore.NewAccountStore(pg)
	keys := postgresstore.NewAPIKeyStore(pg)
	srv := v1.New(v1.Options{
		Auth:             operatorWhenFlagged(),
		PlatformAccounts: accounts,
		RegisterAccounts: accounts,
		// The exact constructor cmd/stellarindex-api uses.
		APIKeyBudgets: v1.NewAPIKeyBudgetStores(keys, rdb, authBackend),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return adminPatchEnv{
		ts:  ts,
		rdb: rdb,
		// Same options buildAPIKeyValidator passes under the redis backend.
		validator: auth.NewRedisAPIKeyValidator(rdb, auth.WithAccountStatus(accounts)),
		accounts:  accounts,
		keys:      keys,
	}
}

type registeredKey struct {
	accountID uuid.UUID
	plaintext string
	keyID     string
	redisKey  string
}

func (e adminPatchEnv) register(t *testing.T) registeredKey {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/register", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("new register request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/register: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/register status = %d; body=%s", resp.StatusCode, body)
	}
	var env struct {
		Data struct {
			AccountID string `json:"account_id"`
			APIKey    string `json:"api_key"`
			KeyID     string `json:"key_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode register response: %v; body=%s", err, body)
	}
	if !strings.HasPrefix(env.Data.APIKey, "sip_") {
		t.Fatalf("register returned no sip_ key; body=%s", body)
	}
	sum := sha256.Sum256([]byte(env.Data.APIKey))
	return registeredKey{
		accountID: uuid.MustParse(env.Data.AccountID),
		plaintext: env.Data.APIKey,
		keyID:     env.Data.KeyID,
		redisKey:  cachekeys.APIKey(hex.EncodeToString(sum[:])).String(),
	}
}

func (e adminPatchEnv) patch(t *testing.T, id uuid.UUID, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch,
		e.ts.URL+"/v1/admin/accounts/"+id.String(), strings.NewReader(body))
	if err != nil {
		t.Fatalf("new PATCH request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Reason", "integration: admin PATCH vs register credential")
	req.Header.Set(operatorHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH %s status = %d; body=%s", body, resp.StatusCode, b)
	}
}

// mustStillAuthenticate is the customer-visible property: the key the
// customer was handed at registration still authenticates, and its
// canonical record still exists with its idle TTL.
func (e adminPatchEnv) mustStillAuthenticate(ctx context.Context, t *testing.T, k registeredKey, after string) {
	t.Helper()
	n, err := e.rdb.Exists(ctx, k.redisKey).Result()
	if err != nil {
		t.Fatalf("redis EXISTS: %v", err)
	}
	if n != 1 {
		t.Fatalf("after %s: the canonical credential record %s was DELETED from Redis — under "+
			"auth_backend=redis that is the customer's key, not a cache entry, and nothing can rebuild it",
			after, k.redisKey)
	}
	sub, err := e.validator.Lookup(ctx, k.plaintext)
	if err != nil {
		t.Fatalf("after %s: register-minted key no longer authenticates: %v", after, err)
	}
	if sub.KeyID != k.keyID {
		t.Fatalf("after %s: authenticated as key %q, want %q", after, sub.KeyID, k.keyID)
	}
	ttl, err := e.rdb.TTL(ctx, k.redisKey).Result()
	if err != nil {
		t.Fatalf("redis TTL: %v", err)
	}
	if ttl <= 0 {
		t.Errorf("after %s: mirror record lost its idle TTL (ttl=%v); it must stay bounded", after, ttl)
	}
}

func TestAdminAccountPatch_PreservesRegisterCredential_RedisBackend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rdb := startPlainRedis(ctx, t)

	// "redis" is the shipped default and what production runs.
	env := newAdminPatchEnv(t, db, rdb, "redis")

	t.Run("override raise", func(t *testing.T) {
		k := env.register(t)
		env.mustStillAuthenticate(ctx, t, k, "registration")

		// The workflow /v1/register's own doc advertises: promote the
		// account to partner-style limits.
		env.patch(t, k.accountID, `{"rate_limit_per_min_override":20000}`)
		env.mustStillAuthenticate(ctx, t, k, "a rate-limit override raise")

		env.patch(t, k.accountID, `{"monthly_request_quota_override":5000000}`)
		env.mustStillAuthenticate(ctx, t, k, "a monthly-quota override raise")

		acct, err := env.accounts.Get(ctx, k.accountID)
		if err != nil {
			t.Fatalf("reload account: %v", err)
		}
		if acct.RateLimitPerMinOverride != 20000 || acct.MonthlyRequestQuotaOverride != 5000000 {
			t.Fatalf("overrides not persisted: rate=%d quota=%d",
				acct.RateLimitPerMinOverride, acct.MonthlyRequestQuotaOverride)
		}
	})

	t.Run("tier lowering clamps in place", func(t *testing.T) {
		k := env.register(t)
		// Promote, lift the key's own budget the way `upgrade-key` does,
		// then demote: the clamp must LOWER the canonical record, not
		// delete it.
		env.patch(t, k.accountID, `{"tier":"partner"}`)
		store := auth.NewRedisAPIKeyStore(rdb)
		if _, err := store.UpdateRateLimit(ctx, k.keyID, 50000); err != nil {
			t.Fatalf("lift key budget: %v", err)
		}
		// Lift the Postgres management row too. The clamp's eviction
		// seam fires only for a row it actually lowered, so without this
		// the subtest never reaches the DEL it exists to guard.
		row, err := env.keys.Get(ctx, k.keyID)
		if err != nil {
			t.Fatalf("load management row: %v", err)
		}
		row.RateLimitPerMin = 50000
		if err := env.keys.Update(ctx, row); err != nil {
			t.Fatalf("lift management row budget: %v", err)
		}
		env.patch(t, k.accountID, `{"tier":"free"}`)

		lowered, err := env.keys.Get(ctx, k.keyID)
		if err != nil {
			t.Fatalf("reload management row: %v", err)
		}
		if want := platform.TierFree.MaxRateLimitPerMin(); lowered.RateLimitPerMin != want {
			t.Fatalf("management row budget = %d, want %d — the clamp (and so its eviction seam) never ran",
				lowered.RateLimitPerMin, want)
		}

		n, err := rdb.Exists(ctx, k.redisKey).Result()
		if err != nil || n != 1 {
			t.Fatalf("after a tier lowering: canonical credential record deleted (exists=%d err=%v)", n, err)
		}
		sub, err := env.validator.Lookup(ctx, k.plaintext)
		if err != nil {
			t.Fatalf("after a tier lowering: register-minted key no longer authenticates: %v", err)
		}
		if want := platform.TierFree.MaxRateLimitPerMin(); sub.RateLimitPerMin != want {
			t.Errorf("clamped budget = %d, want the free ceiling %d", sub.RateLimitPerMin, want)
		}
	})

	t.Run("suspend then reinstate is recoverable", func(t *testing.T) {
		k := env.register(t)
		env.patch(t, k.accountID, `{"status":"suspended","suspended_reason":"integration: temporary hold"}`)

		// Suspension is enforced by the validator's account-status gate
		// (a fresh validator has no cached status, so this is immediate).
		gate := auth.NewRedisAPIKeyValidator(rdb, auth.WithAccountStatus(env.accounts))
		if _, err := gate.Lookup(ctx, k.plaintext); err == nil {
			t.Fatal("a suspended account's key must not authenticate")
		}

		env.patch(t, k.accountID, `{"status":"active"}`)
		reinstated := auth.NewRedisAPIKeyValidator(rdb, auth.WithAccountStatus(env.accounts))
		if _, err := reinstated.Lookup(ctx, k.plaintext); err != nil {
			t.Fatalf("after reinstating the account the ORIGINAL key must work again — a suspension "+
				"that deletes the credential is irreversible: %v", err)
		}
	})
}
