//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Stellar-Index/StellarIndex/internal/accounterasure"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
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
		if err := env.keys.Update(ctx, row.AccountID, row); err != nil {
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

// apiWrite is one table's write privileges for stellarindex_api.
type apiWrite struct{ insert, update, delete bool }

var (
	writeIUD = apiWrite{true, true, true}
	// apiWritableTables is the exact write set 0213 grants stellarindex_api.
	apiWritableTables = map[string]apiWrite{
		"accounts": writeIUD, "api_keys": writeIUD, "api_usage_events": writeIUD,
		"audit_log": {insert: true, update: true}, "customer_webhooks": writeIUD,
		"erased_account_slugs": {insert: true}, "fx_fixings": writeIUD,
		"fx_quotes": writeIUD, "invites": writeIUD, "login_code_lockouts": writeIUD,
		"magic_link_tokens": writeIUD, "price_alerts": writeIUD, "sessions": writeIUD,
		"source_entry_counts": writeIUD, "status_notices": writeIUD, "usage_daily": writeIUD,
		"users": writeIUD, "webauthn_credentials": writeIUD, "webhook_deliveries": writeIUD,
	}
)

// dsnAs returns dsn with its user and database replaced; an empty user
// keeps dsn's own.
func dsnAs(t *testing.T, dsn, user, pwd, db string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	if user != "" {
		u.User = url.UserPassword(user, pwd)
	}
	u.Path = "/" + db
	return u.String()
}

func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestAPIRoleGrants_ReadEverythingWriteOnlyPlatform follows production's
// order: migrations run before the role exists, then ansible creates the
// role and calls apply_api_role_grants() as a superuser. The role must read
// every table (a compressed hypertable chunk and a CAGG included), write only
// apiWritableTables, and be refused 42501 on any write to chain or projected
// data, on TRUNCATE and on DDL. A table a later migration creates is
// readable and not writable.
func TestAPIRoleGrants_ReadEverythingWriteOnlyPlatform(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Production's shape: a NOSUPERUSER role owns the database and runs the
	// migrations; a superuser (ansible) creates the API role and grants.
	superDSN := startTimescale(t, ctx)
	const ownerPwd = "owner-fixture" // gitleaks:allow — throwaway container password, not a credential
	const pwd = "api-role-fixture"   // gitleaks:allow — throwaway container password, not a credential
	bootstrap := openDB(t, superDSN)
	mustExec(t, ctx, bootstrap, `CREATE ROLE si_owner LOGIN NOSUPERUSER PASSWORD '`+ownerPwd+`'`)
	mustExec(t, ctx, bootstrap, `CREATE DATABASE si_grants OWNER si_owner`)
	super := openDB(t, dsnAs(t, superDSN, "", "", "si_grants"))
	for _, ext := range []string{"timescaledb", "citext", `"uuid-ossp"`} {
		mustExec(t, ctx, super, `CREATE EXTENSION IF NOT EXISTS `+ext)
	}
	dsn := dsnAs(t, superDSN, "si_owner", ownerPwd, "si_grants")
	applyMigrations(t, dsn)
	owner := openDB(t, dsn)

	var proconfig []string
	mustScan(t, ctx, owner, &proconfig,
		`SELECT coalesce(proconfig, '{}') FROM pg_proc WHERE oid = 'apply_api_role_grants'::regproc`)
	if want := "search_path=pg_catalog, public, pg_temp"; len(proconfig) != 1 || proconfig[0] != want {
		t.Errorf("apply_api_role_grants proconfig = %q, want exactly [%q]", proconfig, want)
	}

	pair := ohlcDustPair{base: "GRNT-" + priceableIssuer, quote: "native"}
	t0 := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour)
	seed(t, owner, ctx, pair, []seedTrade{{off: 0, base: "1000", quote: "5000", usd: "1"}}, t0)
	compressTradesChunks(t, ctx, owner, 1)

	mustExec(t, ctx, super, `CREATE ROLE stellarindex_api LOGIN PASSWORD '`+pwd+`'`)
	mustExec(t, ctx, super, `SELECT apply_api_role_grants()`)

	apiDSN := dsnAs(t, superDSN, "stellarindex_api", pwd, "si_grants")
	api, err := sql.Open("pgx", apiDSN)
	if err != nil {
		t.Fatalf("sql.Open api: %v", err)
	}
	defer api.Close()

	for _, q := range []string{
		`SELECT count(*) FROM trades`,
		`SELECT count(*) FROM prices_1m`,
		`SELECT count(*) FROM accounts`,
	} {
		var n int
		if err := api.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Errorf("api role read %q: %v", q, err)
		}
	}
	var n int
	if err := api.QueryRowContext(ctx, `SELECT count(*) FROM trades`).Scan(&n); err == nil && n != 1 {
		t.Errorf("api role sees %d trades in the compressed chunk, want 1", n)
	}

	rdb, _ := startRedis(t, ctx)
	exerciseAPIWritePaths(t, ctx, api, apiDSN, rdb)

	for name, stmt := range map[string]string{
		"insert trades":            `INSERT INTO trades SELECT * FROM trades LIMIT 1`,
		"update trades":            `UPDATE trades SET usd_volume = 0`,
		"delete trades":            `DELETE FROM trades`,
		"delete ingestion_cursors": `DELETE FROM ingestion_cursors`,
		"update oracle_updates":    `UPDATE oracle_updates SET price = 0`,
		"delete asset_volume_24h":  `DELETE FROM asset_volume_24h`,
		"truncate accounts":        `TRUNCATE accounts CASCADE`,
		"delete audit_log":         `DELETE FROM audit_log`,
		"update erased slugs":      `UPDATE erased_account_slugs SET slug_sha256 = slug_sha256`,
		"delete erased slugs":      `DELETE FROM erased_account_slugs`,
		"create table":             `CREATE TABLE api_role_owned (x int)`,
		"regrant itself":           `SELECT apply_api_role_grants()`,
	} {
		requireInsufficientPrivilege(t, ctx, api, name, stmt)
	}

	// Every public relation: SELECT held; writes held exactly on the list.
	rows, err := owner.QueryContext(ctx, `
		SELECT c.relname,
		       has_table_privilege('stellarindex_api', c.oid, 'SELECT'),
		       has_table_privilege('stellarindex_api', c.oid, 'INSERT'),
		       has_table_privilege('stellarindex_api', c.oid, 'UPDATE'),
		       has_table_privilege('stellarindex_api', c.oid, 'DELETE'),
		       has_table_privilege('stellarindex_api', c.oid, 'TRUNCATE')
		  FROM pg_class c JOIN pg_namespace ns ON ns.oid = c.relnamespace
		 WHERE ns.nspname = 'public' AND c.relkind IN ('r', 'p', 'v', 'm')`)
	if err != nil {
		t.Fatalf("read privileges: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var rel string
		var sel, ins, upd, del, trunc bool
		if err := rows.Scan(&rel, &sel, &ins, &upd, &del, &trunc); err != nil {
			t.Fatal(err)
		}
		seen[rel] = true
		want := apiWritableTables[rel]
		if got := (apiWrite{ins, upd, del}); !sel || got != want || trunc {
			t.Errorf("%s: select=%v writes=%+v truncate=%v, want select, writes=%+v, no truncate",
				rel, sel, got, trunc, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for rel := range apiWritableTables {
		if !seen[rel] {
			t.Errorf("granted table %s does not exist in public", rel)
		}
	}

	// USAGE exactly on the sequences the write tables own; SELECT on none.
	var badSeqs []string
	if err := owner.QueryRowContext(ctx, `
		WITH owned AS (
		    SELECT d.objid FROM pg_depend d JOIN pg_class o ON o.oid = d.refobjid
		     WHERE d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass
		       AND d.deptype IN ('a', 'i') AND o.relname = ANY ($1))
		SELECT coalesce(array_agg(s.relname::text ORDER BY s.relname), '{}')
		  FROM pg_class s JOIN pg_namespace ns ON ns.oid = s.relnamespace
		 WHERE ns.nspname = 'public' AND s.relkind = 'S'
		   AND (has_sequence_privilege('stellarindex_api', s.oid, 'SELECT')
		        OR has_sequence_privilege('stellarindex_api', s.oid, 'USAGE')
		           <> (s.oid IN (SELECT objid FROM owned)))`,
		mapKeys(apiWritableTables)).Scan(&badSeqs); err != nil {
		t.Fatalf("read sequence privileges: %v", err)
	}
	if len(badSeqs) > 0 {
		t.Errorf("sequences with the wrong stellarindex_api privilege: %v", badSeqs)
	}

	// A table created after the grants, as a later migration would.
	mustExec(t, ctx, owner, `CREATE TABLE api_role_later (x int)`)
	if err := api.QueryRowContext(ctx, `SELECT count(*) FROM api_role_later`).Scan(&n); err != nil {
		t.Errorf("api role read of a later table: %v", err)
	}
	requireInsufficientPrivilege(t, ctx, api, "insert later table", `INSERT INTO api_role_later VALUES (1)`)

	// Down strips the role back to nothing while the role still exists.
	api.Close()
	applyMigrationsUpTo(t, dsn, 212)
	var anyPriv bool
	if err := owner.QueryRowContext(ctx, `
		SELECT has_table_privilege('stellarindex_api', 'accounts', 'SELECT')
		    OR has_table_privilege('stellarindex_api', 'trades', 'SELECT')
		    OR to_regproc('apply_api_role_grants') IS NOT NULL`).Scan(&anyPriv); err != nil {
		t.Fatalf("read privileges after down: %v", err)
	}
	if anyPriv {
		t.Error("0213 down left stellarindex_api a grant or left apply_api_role_grants()")
	}
	var defACLs int
	mustScan(t, ctx, owner, &defACLs, `
		SELECT count(*) FROM pg_default_acl d, aclexplode(d.defaclacl) a
		 WHERE a.grantee = 'stellarindex_api'::regrole`)
	if defACLs != 0 {
		t.Errorf("0213 down left %d pg_default_acl entries granting stellarindex_api", defACLs)
	}
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// exerciseAPIWritePaths drives every store method the API process writes
// through, connected as stellarindex_api. A 42501 here is a grant 0213 is
// missing.
func exerciseAPIWritePaths(t *testing.T, ctx context.Context, api *sql.DB, apiDSN string, rdb *redis.Client) {
	t.Helper()
	check := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("api role %s: %v", what, err)
		}
	}
	store := postgresstore.New(api)

	x := seedErasureAccount(t, ctx, api, rdb, "john", "b.example", "7")
	y := seedErasureAccount(t, ctx, api, rdb, "yves", "d.example", "9")
	accounts := postgresstore.NewAccountStore(store)
	rep, err := (&accounterasure.Eraser{Store: accounts, Redis: rdb}).Erase(ctx, x.accountID, platform.ActorUser)
	check("Erase", err)
	_, err = accounts.RenameUsageSubjects(ctx, postgresstore.UsageSubjects("john"), rep.ErasedSubject)
	check("RenameUsageSubjects", err)

	check("AuditStore.Append", postgresstore.NewAuditStore(store).Append(ctx, platform.AuditEntry{
		AccountID: y.accountID, ActorKind: platform.ActorSystem, Action: "api-role-grant-test",
		Metadata: json.RawMessage(`{"k":"v"}`),
	}))

	tokens := postgresstore.NewTokenStore(store)
	const email = "grants@d.example"
	hash := []byte("grant-token-" + uuid.NewString())
	check("CreateMagicLinkToken", tokens.CreateMagicLinkToken(ctx, platform.MagicLinkToken{
		TokenHash: hash, Email: email, Purpose: platform.TokenPurposeLogin, ExpiresAt: time.Now().Add(time.Hour),
		RequestedIP: net.ParseIP("192.0.2.1"),
	}))
	_, err = tokens.ReserveLoginCode(ctx, email, 5)
	check("ReserveLoginCode", err)
	_, err = tokens.ConsumeMagicLinkToken(ctx, hash)
	check("ConsumeMagicLinkToken", err)
	_, err = tokens.RegisterFailedLoginCode(ctx, email, 3, time.Hour, time.Hour)
	check("RegisterFailedLoginCode", err)
	check("ClearLoginCodeLockout", tokens.ClearLoginCodeLockout(ctx, email))
	_, err = tokens.RegisterFailedLoginCode(ctx, email, 3, time.Hour, time.Hour)
	check("RegisterFailedLoginCode", err)
	_, _, err = tokens.SweepLoginCodeLockouts(ctx, time.Now().Add(time.Hour))
	check("SweepLoginCodeLockouts", err)
	_, err = tokens.SweepExpiredMagicLinkTokens(ctx, time.Now().Add(2*time.Hour))
	check("SweepExpiredMagicLinkTokens", err)

	notices := postgresstore.NewStatusNoticeStore(store)
	n, err := notices.Create(ctx, platform.StatusNotice{Title: "t", Body: "b", Severity: platform.NoticeMinor})
	check("StatusNoticeStore.Create", err)
	if err == nil {
		_, err = notices.Resolve(ctx, n.ID)
		check("StatusNoticeStore.Resolve", err)
	}

	hooks := postgresstore.NewWebhookStore(store)
	var hookID uuid.UUID
	mustScan(t, ctx, api, &hookID, `SELECT id FROM customer_webhooks WHERE account_id = $1`, y.accountID)
	check("EnqueueDelivery", hooks.EnqueueDelivery(ctx, platform.WebhookDelivery{
		WebhookID: hookID, EventType: "price.alert", Payload: json.RawMessage(`{"a":"1"}`),
	}))
	pending, err := hooks.ListPendingDeliveries(ctx, 10)
	check("ListPendingDeliveries", err)
	for _, d := range pending {
		check("MarkDelivered", hooks.MarkDelivered(ctx, d.ID, 200))
	}
	_, err = hooks.SweepFinishedDeliveries(ctx, time.Now().Add(time.Hour))
	check("SweepFinishedDeliveries", err)

	ts, err := timescale.Open(ctx, apiDSN)
	if err != nil {
		t.Fatalf("timescale.Open as api role: %v", err)
	}
	defer func() { _ = ts.Close() }()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	check("InsertFXQuoteBatch", ts.InsertFXQuoteBatch(ctx, []timescale.FXQuote{
		{Bucket: day, Ticker: "EUR", RateUSD: 1.1, Source: "grants-test"},
	}))
	_, err = ts.InsertFXFixingBatch(ctx, []timescale.FXFixing{{
		Ticker: "EUR", Grain: timescale.FXGrainDay, BarStart: day, BarEnd: day.Add(24 * time.Hour),
		RateUSD: "1.1", Source: "grants-test", Generation: 1,
	}})
	check("InsertFXFixingBatch", err)
	check("UpsertUsageDaily", ts.UpsertUsageDaily(ctx, []usage.RollupRow{
		{Day: day.Format("2006-01-02"), Subject: "id:acct:yves", Endpoint: "/v1/price", OK: 1},
	}))
	check("BumpSourceEntryCount", ts.BumpSourceEntryCount(ctx, "grants-test", 1))
}

func requireInsufficientPrivilege(t *testing.T, ctx context.Context, db *sql.DB, name, stmt string) {
	t.Helper()
	_, err := db.ExecContext(ctx, stmt)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("%s: err = %v, want 42501 insufficient_privilege", name, err)
	}
}

const (
	aclTemplatePath  = "configs/ansible/roles/redis-sentinel/templates/users.acl.j2"
	aclIndexPattern  = "~apikey-index:*"
	aclFixtureAppPwd = "index-acl-fixture" // gitleaks:allow — throwaway container ACL user, not a credential
)

// shippedStellarindexACLRule returns the `user stellarindex …` rule of
// the shipped template as ACL SETUSER arguments, with the vaulted
// password placeholder replaced by the fixture word.
func shippedStellarindexACLRule(t *testing.T) []string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", aclTemplatePath))
	if err != nil {
		t.Fatalf("read ACL template: %v", err)
	}
	var rule []string
	inRule := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !inRule && !strings.HasPrefix(trimmed, "user stellarindex ") {
			continue
		}
		inRule = true
		rule = append(rule, strings.TrimSuffix(trimmed, "\\"))
		if !strings.HasSuffix(trimmed, "\\") {
			break
		}
	}
	joined := strings.Join(rule, " ")
	if !strings.Contains(joined, ">{{ redis_password }}") {
		t.Fatalf("template rule has no password placeholder to substitute: %q", joined)
	}
	joined = strings.Replace(joined, ">{{ redis_password }}", ">"+aclFixtureAppPwd, 1)
	fields := strings.Fields(joined)
	if len(fields) < 4 || fields[0] != "user" || fields[1] != "stellarindex" {
		t.Fatalf("could not parse the stellarindex rule out of %s: %q", aclTemplatePath, joined)
	}
	return fields[2:]
}

func withoutPattern(rule []string, pattern string) []string {
	out := make([]string, 0, len(rule))
	for _, f := range rule {
		if f != pattern {
			out = append(out, f)
		}
	}
	return out
}

func setACLUser(ctx context.Context, t *testing.T, admin *redis.Client, user string, rule []string) {
	t.Helper()
	args := []any{"ACL", "SETUSER", user, "reset"}
	for _, f := range rule {
		args = append(args, f)
	}
	if err := admin.Do(ctx, args...).Err(); err != nil {
		t.Fatalf("ACL SETUSER %s: %v", user, err)
	}
}

func appClient(t *testing.T, admin *redis.Client, user string) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: admin.Options().Addr, Username: user, Password: aclFixtureAppPwd})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestAPIKeyIndex_RealRedisACL(t *testing.T) {
	ctx := context.Background()
	admin := startPlainRedis(ctx, t)
	shipped := shippedStellarindexACLRule(t)
	index := cachekeys.APIKeyIndex().String()

	t.Run("the shipped template admits the index family", func(t *testing.T) {
		found := false
		for _, f := range shipped {
			found = found || f == aclIndexPattern
		}
		if !found {
			t.Fatalf("%s does not grant %s: every index access is NOPERM under lockdown and the keyspace walk never retires",
				aclTemplatePath, aclIndexPattern)
		}
	})

	t.Run("ACL not yet applied: issuance and revocation work, nothing half-written", func(t *testing.T) {
		if err := admin.FlushAll(ctx).Err(); err != nil {
			t.Fatalf("flushall: %v", err)
		}
		setACLUser(ctx, t, admin, "stellarindex", withoutPattern(shipped, aclIndexPattern))
		app := appClient(t, admin, "stellarindex")
		store := auth.NewRedisAPIKeyStore(app)
		validator := auth.NewRedisAPIKeyValidator(app)

		// The denial really is in force, and really reads NOPERM.
		err := app.HGet(ctx, index, "ready").Err()
		if err == nil || errors.Is(err, redis.Nil) || !strings.Contains(err.Error(), "NOPERM") {
			t.Fatalf("index read under the old ACL = %v, want a NOPERM denial", err)
		}

		const owner = "account:acl-gap"
		first, firstPlain, err := store.Create(ctx, auth.CreateAPIKeyRequest{Identifier: owner})
		if err != nil {
			t.Fatalf("Create while the index family is denied: %v — key issuance is DOWN until ansible runs", err)
		}
		second, secondPlain, err := store.Create(ctx, auth.CreateAPIKeyRequest{Identifier: owner})
		if err != nil {
			t.Fatalf("second Create: %v", err)
		}
		if _, err := validator.Lookup(ctx, firstPlain); err != nil {
			t.Fatalf("key issued under the old ACL does not authenticate: %v", err)
		}
		if n, err := admin.Exists(ctx, index).Result(); err != nil || n != 0 {
			t.Fatalf("index key exists=%d err=%v after denied issuance: the script ran past the denial", n, err)
		}
		if n, err := admin.DBSize(ctx).Result(); err != nil || n != 2 {
			t.Fatalf("dbsize=%d err=%v, want exactly the 2 credential records", n, err)
		}

		recs, err := store.ListKeysForIdentifier(ctx, owner)
		if err != nil || len(recs) != 2 {
			t.Fatalf("list under the old ACL: %d records, err=%v; want 2", len(recs), err)
		}
		if err := store.RevokeKeyByID(ctx, owner, first.KeyID); err != nil {
			t.Fatalf("revoke under the old ACL: %v", err)
		}
		if _, err := validator.Lookup(ctx, firstPlain); err == nil {
			t.Fatal("revoked key still authenticates under the old ACL: revocation silently no-ops")
		}

		// Ansible applies the template: the pattern is granted to the live
		// user. The API process is NOT restarted — same store, same pool —
		// so this also proves no restart is needed to retire the walk.
		if err := admin.Do(ctx, "ACL", "SETUSER", "stellarindex", aclIndexPattern).Err(); err != nil {
			t.Fatalf("grant %s: %v", aclIndexPattern, err)
		}
		recs, err = store.ListKeysForIdentifier(ctx, owner)
		if err != nil || len(recs) != 1 || recs[0].KeyID != second.KeyID {
			t.Fatalf("list after the ACL is applied = %+v, err=%v; want the key minted during the gap", recs, err)
		}
		if ready, err := admin.HGet(ctx, index, "ready").Result(); err != nil || ready != "1" {
			t.Fatalf("index ready=%q err=%v after the first lookup under the shipped ACL", ready, err)
		}
		if err := store.RevokeKeyByID(ctx, owner, second.KeyID); err != nil {
			t.Fatalf("revoke after the ACL is applied: %v", err)
		}
		if _, err := validator.Lookup(ctx, secondPlain); err == nil {
			t.Fatal("a key minted during the ACL gap survives revocation once the index is live")
		}
	})

	t.Run("shipped ACL: indexed issuance, lookups and revoke, surviving a script-cache flush", func(t *testing.T) {
		if err := admin.FlushAll(ctx).Err(); err != nil {
			t.Fatalf("flushall: %v", err)
		}
		setACLUser(ctx, t, admin, "stellarindex", shipped)
		app := appClient(t, admin, "stellarindex")
		store := auth.NewRedisAPIKeyStore(app)

		const owner = "account:steady-state"
		if _, err := store.ListKeysForIdentifier(ctx, owner); err != nil { // builds on an empty deployment
			t.Fatalf("first lookup: %v", err)
		}
		rec, plaintext, err := store.Create(ctx, auth.CreateAPIKeyRequest{Identifier: owner})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Every Redis restart or failover empties the script cache.
		if err := admin.ScriptFlush(ctx).Err(); err != nil {
			t.Fatalf("script flush: %v", err)
		}
		mirrored := auth.MirroredKey{
			Plaintext: "sip_" + strings.Repeat("cd", 32),
			Record:    auth.APIKeyRecord{KeyID: "kid_acl_mirror", Identifier: owner},
		}
		if err := store.CreateWithSecret(ctx, mirrored); err != nil {
			t.Fatalf("CreateWithSecret after SCRIPT FLUSH: %v", err)
		}
		if ptr, err := admin.HGet(ctx, index, "k:"+rec.KeyID).Result(); err != nil || len(ptr) != 64 {
			t.Fatalf("KeyID pointer = %q err=%v, want the record's sha256 hex", ptr, err)
		}
		if ttl, err := admin.TTL(ctx, index).Result(); err != nil || ttl >= 0 {
			t.Fatalf("index ttl=%v err=%v, want none", ttl, err)
		}
		recs, err := store.ListKeysForIdentifier(ctx, owner)
		if err != nil || len(recs) != 2 {
			t.Fatalf("list: %d records, err=%v; want 2", len(recs), err)
		}
		if _, err := store.UpdateRateLimit(ctx, rec.KeyID, 42); err != nil {
			t.Fatalf("UpdateRateLimit: %v", err)
		}
		if err := store.RevokeKeyByID(ctx, owner, rec.KeyID); err != nil {
			t.Fatalf("RevokeKeyByID: %v", err)
		}
		if _, err := auth.NewRedisAPIKeyValidator(app).Lookup(ctx, plaintext); err == nil {
			t.Fatal("revoked key still authenticates")
		}
		if n, err := admin.HExists(ctx, index, "k:"+rec.KeyID).Result(); err != nil || n {
			t.Fatalf("KeyID pointer survived the revoke (exists=%v err=%v)", n, err)
		}
	})
}

// The store half of the durable per-email
// code-guess lockout (migration 0122).
//
// The handler half lives in
// internal/api/v1/dashboardauth/login_lockout_test.go and proves the policy:
// a token re-mint no longer hands out a fresh guess budget. This proves the
// three things only a real database can:
//
//  1. the counter is DURABLE — it is a row, not a Redis key with a TTL, so a
//     cache flush cannot clear it (which is the entire finding);
//  2. the UPSERT is atomic under concurrency — parallel wrong guesses for one
//     address cannot both read n and both write n+1, which would let a
//     parallel grinder run past the cap;
//  3. the window/lock arithmetic is Postgres', not Go's.
func TestLoginCodeLockout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const (
		maxFailures = 10
		window      = 24 * time.Hour
		lockFor     = 24 * time.Hour
	)
	tokens := postgresstore.NewTokenStore(postgresstore.New(db))

	t.Run("CountsUpToTheCapThenLocks", func(t *testing.T) {
		const email = "grind@example.com"
		for i := 1; i < maxFailures; i++ {
			state, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor)
			if err != nil {
				t.Fatalf("failure %d: %v", i, err)
			}
			if state.FailedCount != i {
				t.Fatalf("failure %d: FailedCount = %d, want %d", i, state.FailedCount, i)
			}
			if state.Locked(time.Now().UTC()) {
				t.Fatalf("locked early at failure %d of %d", i, maxFailures)
			}
		}
		state, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor)
		if err != nil {
			t.Fatalf("capping failure: %v", err)
		}
		if !state.Locked(time.Now().UTC()) {
			t.Fatalf("not locked after %d failures (FailedCount=%d, LockedUntil=%v)",
				maxFailures, state.FailedCount, state.LockedUntil)
		}
		// And a fresh read agrees — the lock is in the row, not in the
		// return value of the call that set it.
		read, err := tokens.LoginCodeLockoutStatus(ctx, email)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if !read.Locked(time.Now().UTC()) {
			t.Error("LoginCodeLockoutStatus does not see the lock the register call reported")
		}
	})

	t.Run("UnknownEmailIsNotLocked", func(t *testing.T) {
		state, err := tokens.LoginCodeLockoutStatus(ctx, "never-seen@example.com")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if state != (platform.LoginCodeLockout{}) {
			t.Errorf("state = %+v, want the zero value for an address with no failures", state)
		}
	})

	t.Run("ClearResetsTheCounter", func(t *testing.T) {
		const email = "cleared@example.com"
		for i := 0; i < maxFailures; i++ {
			if _, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor); err != nil {
				t.Fatalf("failure %d: %v", i, err)
			}
		}
		if err := tokens.ClearLoginCodeLockout(ctx, email); err != nil {
			t.Fatalf("clear: %v", err)
		}
		state, err := tokens.LoginCodeLockoutStatus(ctx, email)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if state.FailedCount != 0 || state.Locked(time.Now().UTC()) {
			t.Errorf("state = %+v after clear, want the zero value", state)
		}
		// Idempotent — a second clear (or one for an unknown address) is
		// not an error; the handler calls it on every successful login.
		if err := tokens.ClearLoginCodeLockout(ctx, email); err != nil {
			t.Errorf("second clear: %v", err)
		}
	})

	// The parallel grinder. Without an atomic UPSERT, N concurrent
	// failures collapse into far fewer counted ones and the cap is
	// effectively raised by whatever concurrency the attacker can muster.
	t.Run("ConcurrentFailuresAllCount", func(t *testing.T) {
		const (
			email   = "parallel@example.com"
			racers  = 12
			bigCap  = 1000 // high enough that no lock arms mid-race
			bigLock = time.Hour
		)
		var wg sync.WaitGroup
		errs := make(chan error, racers)
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := tokens.RegisterFailedLoginCode(ctx, email, bigCap, window, bigLock); err != nil {
					errs <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent register: %v", err)
		}

		state, err := tokens.LoginCodeLockoutStatus(ctx, email)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if state.FailedCount != racers {
			t.Errorf("FailedCount = %d after %d concurrent failures, want %d — "+
				"lost increments let a parallel grinder run past the cap", state.FailedCount, racers, racers)
		}
	})

	// A window that fully elapses without reaching the cap starts fresh:
	// a user who mistypes twice in March must not carry that into June.
	t.Run("ElapsedWindowRestartsTheCount", func(t *testing.T) {
		const email = "stale-window@example.com"
		if _, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor); err != nil {
			t.Fatalf("first failure: %v", err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE login_code_lockouts
			    SET window_started_at = now() - interval '48 hours',
			        failed_count      = 9
			  WHERE email = $1`, email); err != nil {
			t.Fatalf("age the window: %v", err)
		}
		state, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor)
		if err != nil {
			t.Fatalf("post-window failure: %v", err)
		}
		if state.FailedCount != 1 {
			t.Errorf("FailedCount = %d after an elapsed window, want 1 (the window restarts)", state.FailedCount)
		}
		if state.Locked(time.Now().UTC()) {
			t.Error("locked on the first failure of a fresh window")
		}
	})

	// An in-force lock is NOT slid forward by further guessing: otherwise
	// an attacker could hold a victim's code path shut indefinitely by
	// guessing once every 23 hours.
	t.Run("FurtherFailuresDoNotExtendAnActiveLock", func(t *testing.T) {
		const email = "no-extend@example.com"
		for i := 0; i < maxFailures; i++ {
			if _, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor); err != nil {
				t.Fatalf("failure %d: %v", i, err)
			}
		}
		first, err := tokens.LoginCodeLockoutStatus(ctx, email)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if _, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor); err != nil {
			t.Fatalf("extra failure: %v", err)
		}
		after, err := tokens.LoginCodeLockoutStatus(ctx, email)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if !after.LockedUntil.Equal(first.LockedUntil) {
			t.Errorf("LockedUntil moved from %v to %v — a grinder can hold the lock open forever",
				first.LockedUntil, after.LockedUntil)
		}
	})

	// ── Retention sweep ──
	//
	// `login_code_lockouts.email` is ATTACKER-CHOSEN: POST
	// /v1/auth/verify-code is unauthenticated and accepts any well-formed
	// address, so one wrong guess against a synthetic address inserts a
	// row that ClearLoginCodeLockout can never remove (nobody can sign in
	// as an address that does not exist). Bounded only by the anonymous
	// rate limit, that is a slow remote table-fill on a disk-fixed host.
	//
	// The sweep is what bounds it — and the predicate has to be exactly
	// right in both directions: reap settled rows, never touch a live
	// lock.
	t.Run("SweepReapsSettledRowsOnly", func(t *testing.T) {
		const retention = 48 * time.Hour
		type fixture struct {
			email       string
			ageHours    int
			lockedHours int // >0 = locked into the future, <0 = lock expired that long ago
			wantReaped  bool
		}
		fixtures := []fixture{
			// The attack residue: old, never locked, nobody owns it.
			{email: "sweep-old-unlocked@example.com", ageHours: 72, wantReaped: true},
			// Old and its lock has since expired — settled, reapable.
			{email: "sweep-old-expired-lock@example.com", ageHours: 72, lockedHours: -1, wantReaped: true},
			// Old but STILL LOCKED. Must survive at any age: reaping it
			// would hand a grinder an early release.
			{email: "sweep-old-live-lock@example.com", ageHours: 72, lockedHours: 6, wantReaped: false},
			// Recent, inside retention — a real user's in-progress
			// counting window.
			{email: "sweep-fresh@example.com", ageHours: 1, wantReaped: false},
			// Exactly at the boundary, on the keep side.
			{email: "sweep-boundary@example.com", ageHours: 47, wantReaped: false},
		}
		for _, f := range fixtures {
			if _, err := tokens.RegisterFailedLoginCode(ctx, f.email, maxFailures, window, lockFor); err != nil {
				t.Fatalf("seed %s: %v", f.email, err)
			}
			if _, err := db.ExecContext(ctx,
				`UPDATE login_code_lockouts
				    SET updated_at   = now() - make_interval(hours => $2),
				        locked_until = CASE WHEN $3 = 0 THEN NULL
				                            ELSE now() + make_interval(hours => $3) END
				  WHERE email = $1`,
				f.email, f.ageHours, f.lockedHours); err != nil {
				t.Fatalf("age %s: %v", f.email, err)
			}
		}

		// A row in a NEIGHBOURING table with the same shape of age — the
		// sweep must not widen into it.
		if _, err := db.ExecContext(ctx,
			`INSERT INTO magic_link_tokens (token_hash, email, purpose, expires_at, requested_ip, created_at)
			 VALUES ($1, $2, 'login', now() + interval '15 minutes', '203.0.113.7', now() - interval '72 hours')`,
			[]byte("sweep-bystander-token-hash-32byt"), "bystander@example.com"); err != nil {
			t.Fatalf("seed bystander token: %v", err)
		}

		deleted, _, err := tokens.SweepLoginCodeLockouts(ctx, time.Now().UTC().Add(-retention))
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if deleted != 2 {
			t.Errorf("deleted = %d, want 2 (the two settled rows only)", deleted)
		}

		for _, f := range fixtures {
			state, err := tokens.LoginCodeLockoutStatus(ctx, f.email)
			if err != nil {
				t.Fatalf("status %s: %v", f.email, err)
			}
			gone := state == (platform.LoginCodeLockout{})
			if gone != f.wantReaped {
				verb := map[bool]string{true: "reaped", false: "kept"}
				t.Errorf("%s was %s, want %s (age=%dh locked=%dh)",
					f.email, verb[gone], verb[f.wantReaped], f.ageHours, f.lockedHours)
			}
		}

		var bystanders int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM magic_link_tokens WHERE email = $1`,
			"bystander@example.com").Scan(&bystanders); err != nil {
			t.Fatalf("count bystander: %v", err)
		}
		if bystanders != 1 {
			t.Errorf("magic_link_tokens rows for the bystander = %d, want 1 — the sweep reached into another table",
				bystanders)
		}
	})

	// The gauge's source. A count that does not see the rows makes the
	// growth signal useless.
	t.Run("CountSeesTheRows", func(t *testing.T) {
		before, err := tokens.CountLoginCodeLockouts(ctx)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		for _, email := range []string{"count-a@example.com", "count-b@example.com"} {
			if _, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor); err != nil {
				t.Fatalf("seed %s: %v", email, err)
			}
		}
		after, err := tokens.CountLoginCodeLockouts(ctx)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if after != before+2 {
			t.Errorf("count = %d, want %d", after, before+2)
		}
	})

	// The sweep's driving column must be indexed: the table's size is
	// attacker-influenced, so a seq scan here is a second-order DoS.
	t.Run("SweepPredicateIsIndexed", func(t *testing.T) {
		var exists bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
			    SELECT 1 FROM pg_indexes
			     WHERE tablename = 'login_code_lockouts'
			       AND indexdef ILIKE '%(updated_at)%'
			)`).Scan(&exists); err != nil {
			t.Fatalf("check index: %v", err)
		}
		if !exists {
			t.Error("no index leading with updated_at — the retention sweep seq-scans a table an unauthenticated caller can grow")
		}
	})

	// Durability, stated plainly: the state is a row. This is the whole
	// point of the finding — a Redis-backed bound lived in Redis and a flush
	// cleared it.
	t.Run("StateIsARowNotACacheEntry", func(t *testing.T) {
		const email = "durable@example.com"
		if _, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor); err != nil {
			t.Fatalf("failure: %v", err)
		}
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT failed_count FROM login_code_lockouts WHERE email = $1`, email).Scan(&count); err != nil {
			t.Fatalf("read the row directly: %v", err)
		}
		if count != 1 {
			t.Errorf("login_code_lockouts.failed_count = %d, want 1", count)
		}
	})
}

// startPlainRedis is startRedis without the `--save 1 1` flag that
// test exists to weaponise: these tests want an ordinary server, and a
// BGSAVE firing after every write during the concurrency subtest would
// add timing noise to the one assertion that depends on real
// contention.
func startPlainRedis(ctx context.Context, t *testing.T) *redis.Client {
	t.Helper()
	ctr, err := testcontainers.Run(ctx,
		"redis:7.4-alpine",
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("Ready to accept connections").
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("docker unavailable, skipping integration test: %v", err)
		}
		t.Fatalf("start redis: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatalf("container mapped port: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: fmt.Sprintf("%s:%s", host, port.Port())})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	return rdb
}

// frozenClock pins the window suffix so every observation below is
// about the KEY's lifetime rather than about the bucket rolling over.
// A test that let the clock advance would see a fresh key and could
// not tell "the TTL drained" from "we moved to the next window".
func frozenClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// TestRatelimit_RealRedis is the whole real-Redis surface for
// internal/ratelimit, sharing one container across subtests (the
// clickhouse_harness_test.go economy, applied at test scope).
func TestRatelimit_RealRedis(t *testing.T) {
	ctx := context.Background()
	rdb := startPlainRedis(ctx, t)

	t.Run("fixed_window_drain_ttl_actually_expires", func(t *testing.T) {
		testFixedWindowDrainTTL(ctx, t, rdb)
	})
	t.Run("fixed_window_ttl_is_not_re_armed_by_later_increments", func(t *testing.T) {
		testFixedWindowTTLNotReArmed(ctx, t, rdb)
	})
	t.Run("fixed_window_unset_key_race_is_atomic", func(t *testing.T) {
		testFixedWindowUnsetKeyRace(ctx, t, rdb)
	})
	t.Run("bucket_survives_script_cache_flush", func(t *testing.T) {
		testBucketSurvivesScriptFlush(ctx, t, rdb)
	})
	t.Run("bucket_deny_carries_a_real_ttl_shaped_reply", func(t *testing.T) {
		testBucketDenyReplyShape(ctx, t, rdb)
	})
}

// testFixedWindowDrainTTL proves the 2×window drain TTL is a real
// server-side expiry: with the clock frozen — so the key suffix cannot
// move — the counter key must vanish on its own and the next Incr must
// start again at 1. miniredis never expires anything without an
// explicit FastForward, so this property is asserted nowhere else.
func testFixedWindowDrainTTL(ctx context.Context, t *testing.T, rdb *redis.Client) {
	t.Helper()
	at := time.Unix(1_700_000_000, 0).UTC()
	base := "itest-drain:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	key := fmt.Sprintf("%s:%d", base, at.Unix()) // window = 1s ⇒ suffix = unix seconds

	c := ratelimit.NewFixedWindowCounter(rdb, time.Second, frozenClock(at))

	n, err := c.Incr(ctx, base)
	if err != nil {
		t.Fatalf("incr: %v", err)
	}
	if n != 1 {
		t.Fatalf("first Incr = %d, want 1 — INCR on a missing key must create it at 1", n)
	}
	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	// 2×1s window. Redis reports whole seconds and has already spent a
	// few ms, so 2s is the ceiling and anything above zero the floor.
	if ttl <= 0 || ttl > 2*time.Second {
		t.Fatalf("TTL(%s) = %v, want (0, 2s] — the drain TTL must be set by the "+
			"creating INCR (REL-05)", key, ttl)
	}

	// Wait past the drain TTL in real wall-clock time. Nothing
	// fast-forwards a real server.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		exists, existsErr := rdb.Exists(ctx, key).Result()
		if existsErr != nil {
			t.Fatalf("exists: %v", existsErr)
		}
		if exists == 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if exists, existsErr := rdb.Exists(ctx, key).Result(); existsErr != nil || exists != 0 {
		t.Fatalf("key %s still present after the drain TTL (exists=%d, err=%v) — a "+
			"counter key that outlives its TTL leaks permanently, because the window "+
			"suffix moves on and nothing ever revisits it", key, exists, existsErr)
	}

	n, err = c.Incr(ctx, base)
	if err != nil {
		t.Fatalf("incr after drain: %v", err)
	}
	if n != 1 {
		t.Fatalf("Incr after drain = %d, want 1 — a drained key must be recreated at 1", n)
	}
}

// testFixedWindowTTLNotReArmed pins the `if current == 1` guard in
// incrLua. Without it the TTL would be re-armed on every increment and
// a key under sustained load would never expire — the same unbounded
// namespace, arrived at from the other direction.
func testFixedWindowTTLNotReArmed(ctx context.Context, t *testing.T, rdb *redis.Client) {
	t.Helper()
	at := time.Unix(1_700_000_000, 0).UTC()
	base := "itest-noreset:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	// window = 10s ⇒ suffix = unix/10, TTL = 20s.
	key := fmt.Sprintf("%s:%d", base, at.Unix()/10)

	c := ratelimit.NewFixedWindowCounter(rdb, 10*time.Second, frozenClock(at))
	if _, err := c.Incr(ctx, base); err != nil {
		t.Fatalf("incr: %v", err)
	}
	first, err := rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("pttl: %v", err)
	}

	// The margin has to dominate round-trip jitter, not merely exceed
	// it. An earlier revision of this test slept 1.5s and asserted only
	// `second < first`; under a deliberately broken script (EXPIRE
	// re-armed every call) it still passed roughly half the time,
	// because it was really comparing two EVAL+PTTL round-trips a
	// couple of milliseconds apart. Sleep well past the noise floor and
	// require the TTL to have DECAYED by most of the elapsed time.
	const (
		dwell     = 3 * time.Second
		minDecay  = 2 * time.Second // < dwell, to absorb second-rounding
		ttlWindow = 10 * time.Second
	)
	time.Sleep(dwell)

	if _, err := c.Incr(ctx, base); err != nil {
		t.Fatalf("second incr: %v", err)
	}
	second, err := rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("pttl after second incr: %v", err)
	}
	if first-second < minDecay {
		t.Fatalf("PTTL went %v → %v across a second increment %v later — it decayed by "+
			"only %v, so the increment re-armed it. The drain TTL must be set ONLY by "+
			"the increment that CREATES the key (incrLua's `current == 1` guard); a "+
			"re-armed TTL makes a hot counter key immortal.",
			first, second, dwell, first-second)
	}
	// Sanity: the whole assertion above is meaningless if the key had
	// already expired and been recreated at a full TTL.
	if second <= 0 || second > 2*ttlWindow {
		t.Fatalf("PTTL after second incr = %v, want (0, %v] — the key must still be the "+
			"original one, mid-drain", second, 2*ttlWindow)
	}
	// And it must still be counting up, not restarting.
	n, err := c.Incr(ctx, base)
	if err != nil {
		t.Fatalf("third incr: %v", err)
	}
	if n != 3 {
		t.Fatalf("third Incr = %d, want 3 — increments inside one window accumulate", n)
	}
}

// testFixedWindowUnsetKeyRace is the EXPIRE race the Lua fold exists for,
// observed against a real server: many goroutines over several
// INDEPENDENT clients (separate connection pools, so the contention is
// genuinely server-side) hit one missing key inside one window.
//
// Real Redis executes each EVAL to completion before the next, so the
// post-increment counts must be exactly the permutation 1..N — no
// duplicates, no gaps — and exactly one caller may observe 1. And
// whichever caller that was, the key must carry a TTL afterwards: a
// TTL-less counter key is the permanent leak.
func testFixedWindowUnsetKeyRace(ctx context.Context, t *testing.T, rdb *redis.Client) {
	t.Helper()
	const (
		clients = 4
		perGo   = 25
		total   = clients * perGo
	)
	at := time.Unix(1_700_000_000, 0).UTC()
	base := "itest-race:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	key := fmt.Sprintf("%s:%d", base, at.Unix()/60) // window = 60s

	addr := rdb.Options().Addr
	counts := make([]int64, total)
	var wg sync.WaitGroup
	errs := make(chan error, total)
	start := make(chan struct{})

	for ci := range clients {
		client := redis.NewClient(&redis.Options{Addr: addr})
		t.Cleanup(func() { _ = client.Close() })
		c := ratelimit.NewFixedWindowCounter(client, time.Minute, frozenClock(at))
		for gi := range perGo {
			slot := ci*perGo + gi
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				n, err := c.Incr(ctx, base)
				if err != nil {
					errs <- err
					return
				}
				counts[slot] = n
			}()
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent incr: %v", err)
	}

	seen := make(map[int64]int, total)
	for _, n := range counts {
		seen[n]++
	}
	for want := int64(1); want <= total; want++ {
		if seen[want] != 1 {
			t.Fatalf("post-increment count %d was observed %d times (want exactly 1); "+
				"counts across %d concurrent callers must be the permutation 1..%d — "+
				"a duplicate is a lost update and would let a throttled caller past its cap",
				want, seen[want], total, total)
		}
	}

	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("TTL(%s) = %v after a %d-way race on an unset key; the caller that "+
			"created the key must have set the drain TTL in the same EVAL (REL-05) — "+
			"a TTL-less key is never revisited and leaks permanently", key, ttl, total)
	}
}

// testBucketSurvivesScriptFlush models a Redis restart / failover: the
// server's script cache is empty, so the EVALSHA go-redis sends first
// misses with NOSCRIPT and must be retried as a full EVAL. miniredis
// cannot produce that miss, so nothing else in the tree covers it —
// and the production consequence is total: every Take() after a Redis
// restart would error, fail open for DefaultDwellTime, then fail
// CLOSED (503) for the whole API.
func testBucketSurvivesScriptFlush(ctx context.Context, t *testing.T, rdb *redis.Client) {
	t.Helper()
	at := time.Unix(1_700_000_000, 0).UTC()
	key := "itest-flush:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	b := ratelimit.New(rdb, 10, time.Minute,
		ratelimit.WithClock(frozenClock(at)),
		ratelimit.WithKeyPrefix("itest-rl:"),
	)

	res, err := b.Take(ctx, key)
	if err != nil {
		t.Fatalf("take before flush: %v", err)
	}
	if !res.Allowed || res.Count != 1 {
		t.Fatalf("take before flush = %+v, want allowed with count 1", res)
	}

	if err := rdb.ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("script flush: %v", err)
	}

	res, err = b.Take(ctx, key)
	if err != nil {
		t.Fatalf("take after SCRIPT FLUSH: %v — the limiter must recover from an empty "+
			"server-side script cache (Redis restart / failover), not error every call", err)
	}
	if !res.Allowed || res.Count != 2 {
		t.Fatalf("take after SCRIPT FLUSH = %+v, want allowed with count 2 — the counter "+
			"must continue from the surviving key, not restart", res)
	}
}

// testBucketDenyReplyShape drives the bucket over its limit against a
// real server and pins the deny path end to end. The Lua returns
// `{current, TTL}`; the Go side asserts `[]any{int64, int64}` and
// derives Allowed from `count`, never from the TTL. Real Redis RESP
// conversion is the authority for that shape, and the deny arm is the
// only one that calls TTL at all.
func testBucketDenyReplyShape(ctx context.Context, t *testing.T, rdb *redis.Client) {
	t.Helper()
	// 1700000000 / 60 = 28333333, ×60 = 1699999980, so the anchor sits
	// 20s into its 60s window and a denial must advertise the
	// remaining 40s.
	const (
		anchorUnix        = 1_700_000_000
		wantRetryAfterSec = 60 - (anchorUnix - (anchorUnix/60)*60)
	)
	at := time.Unix(anchorUnix, 0).UTC()
	key := "itest-deny:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	b := ratelimit.New(rdb, 2, time.Minute,
		ratelimit.WithClock(frozenClock(at)),
		ratelimit.WithKeyPrefix("itest-rl:"),
	)

	for i := 1; i <= 2; i++ {
		res, err := b.Take(ctx, key)
		if err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("take %d = %+v, want allowed (max=2)", i, res)
		}
		if want := 2 - i; res.Remaining != want {
			t.Fatalf("take %d remaining = %d, want %d", i, res.Remaining, want)
		}
	}

	res, err := b.Take(ctx, key)
	if err != nil {
		t.Fatalf("take 3: %v", err)
	}
	if res.Allowed {
		t.Fatalf("take 3 = %+v, want denied — the 3rd request in a max=2 window is over", res)
	}
	if res.Count != 3 {
		t.Fatalf("take 3 count = %d, want 3 (post-increment count is authoritative)", res.Count)
	}
	if res.Remaining != 0 {
		t.Fatalf("take 3 remaining = %d, want 0 (clamped)", res.Remaining)
	}
	// RetryAfter is the seconds left in the WINDOW, not the 2×window
	// drain TTL Redis holds on the key (which would be up to 120s here).
	if want := time.Duration(wantRetryAfterSec) * time.Second; res.RetryAfter != want {
		t.Fatalf("take 3 RetryAfter = %v, want %v — the header must be the remaining "+
			"window, not the drain TTL", res.RetryAfter, want)
	}
}

// TestAPI_EndToEnd is the first integration test that proves the
// HTTP query path works end-to-end:
//
//	Timescale → Store.TradesInRange → v1.HistoryReader adapter
//	  → /v1/history + /v1/vwap + /v1/ohlc + /v1/markets handlers
//
// Catches regressions where unit-test stubs mask real storage /
// schema / adapter drift. Builds the same stack the stellarindex-api
// binary builds (minus Redis, rate limit, SEP-1 metadata).
func TestAPI_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Seed 4 trades of XLM/USDC across a 30-minute window.
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(c.NativeAsset(), usdc)

	// Anchor trades well in the past to make the from/to window math
	// deterministic regardless of the test's wall clock.
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	trades := []c.Trade{
		mkAPITrade(1, t0.Add(0*time.Minute), pair, 1_000_000_000, 12_000_000),
		mkAPITrade(2, t0.Add(10*time.Minute), pair, 1_000_000_000, 12_100_000),
		mkAPITrade(3, t0.Add(20*time.Minute), pair, 1_000_000_000, 12_200_000),
		mkAPITrade(4, t0.Add(30*time.Minute), pair, 1_000_000_000, 12_050_000),
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	// Force-refresh the CAGGs /v1/markets reads so it sees the seeded
	// trades before the 30s policy fires. /v1/markets (DistinctPairs)
	// enumerates pairs from prices_1d (the right-granularity rewrite)
	// and reads 24h volume from prices_1m — refresh BOTH, else the market
	// list is empty even though prices_1m has the rows.
	for _, stmt := range []string{
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, NULL)`,
	} {
		if _, err := store.DB().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("refresh cagg: %v", err)
		}
	}

	// Same force-refresh for pools_per_source_1h (migration 0036 —
	// the durable backing for /v1/pools). Without this the
	// AllPools sub-test sees an empty CAGG and returns zero rows for
	// the seeded trades.
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('pools_per_source_1h', NULL, NULL)`,
	); err != nil {
		t.Fatalf("refresh pools_per_source_1h: %v", err)
	}

	// Build the same v1.Server the stellarindex-api binary builds —
	// minus the adapters we don't need here.
	srv := v1.New(v1.Options{
		History: apiHistoryAdapter{s: store},
		Markets: apiMarketsAdapter{s: store},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Build the window to cover the seeded trades.
	from := t0.Add(-1 * time.Minute).Format(time.RFC3339)
	to := t0.Add(31 * time.Minute).Format(time.RFC3339)
	pairQS := "base=native&quote=USDC-" + "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	windowQS := "&from=" + from + "&to=" + to

	t.Run("/v1/history", func(t *testing.T) {
		var env struct {
			Data []v1.TradeRow `json:"data"`
		}
		getJSON(t, ts.URL+"/v1/history?"+pairQS+windowQS, &env)
		if len(env.Data) != 4 {
			t.Fatalf("history returned %d rows, want 4", len(env.Data))
		}
		// Must be chronological.
		for i := 1; i < len(env.Data); i++ {
			if !env.Data[i-1].Timestamp.Before(env.Data[i].Timestamp) {
				t.Errorf("history not chronological at i=%d: %v >= %v",
					i, env.Data[i-1].Timestamp, env.Data[i].Timestamp)
			}
		}
	})

	t.Run("/v1/ohlc", func(t *testing.T) {
		var env struct {
			Data v1.OHLCBar `json:"data"`
		}
		getJSON(t, ts.URL+"/v1/ohlc?"+pairQS+windowQS, &env)
		// Base=1e9 means price = quote/1e9. Amounts 12.0M to 12.2M →
		// prices 0.012, 0.0121, 0.0122, 0.01205. Open first, close last.
		if env.Data.Open != "0.0120000000" {
			t.Errorf("Open = %q, want 0.0120000000", env.Data.Open)
		}
		if env.Data.Close != "0.0120500000" {
			t.Errorf("Close = %q, want 0.0120500000", env.Data.Close)
		}
		if env.Data.High != "0.0122000000" {
			t.Errorf("High = %q, want 0.0122000000", env.Data.High)
		}
		if env.Data.Low != "0.0120000000" {
			t.Errorf("Low = %q, want 0.0120000000", env.Data.Low)
		}
		if env.Data.TradeCount != 4 {
			t.Errorf("TradeCount = %d, want 4", env.Data.TradeCount)
		}
	})

	t.Run("/v1/vwap", func(t *testing.T) {
		var env struct {
			Data v1.VWAPResult `json:"data"`
		}
		getJSON(t, ts.URL+"/v1/vwap?"+pairQS+windowQS, &env)
		// VWAP = Σ(Q)/Σ(B) = (12M+12.1M+12.2M+12.05M) / (4×1e9)
		//     = 48_350_000 / 4_000_000_000 = 0.012087500...
		if env.Data.Price != "0.0120875000" {
			t.Errorf("Price = %q, want 0.0120875000", env.Data.Price)
		}
		if env.Data.TradeCount != 4 {
			t.Errorf("TradeCount = %d, want 4", env.Data.TradeCount)
		}
	})

	t.Run("/v1/markets", func(t *testing.T) {
		var env struct {
			Data []v1.Market `json:"data"`
		}
		getJSON(t, ts.URL+"/v1/markets", &env)
		if len(env.Data) != 1 {
			t.Fatalf("expected 1 market, got %d", len(env.Data))
		}
		m := env.Data[0]
		if m.Base != "native" || m.Quote != "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" {
			t.Errorf("market pair mismatch: %+v", m)
		}
	})

	t.Run("/v1/history cursor drain", func(t *testing.T) {
		// Walk the 4 seeded trades with limit=1 to exercise cursor
		// pagination end-to-end. Must return all 4 in chronological
		// order with no duplicates and no losses — this is the path
		// that exercises the full-PK tiebreak (otherwise a page
		// break mid-ledger could drop a row).
		var collected []v1.TradeRow
		seenKeys := map[string]bool{}
		cursor := ""
		for page := 0; page < 10; page++ {
			qs := pairQS + windowQS + "&limit=1"
			if cursor != "" {
				qs += "&cursor=" + cursor
			}
			var env struct {
				Data       []v1.TradeRow `json:"data"`
				Pagination *struct {
					Next string `json:"next"`
				} `json:"pagination"`
			}
			getJSON(t, ts.URL+"/v1/history?"+qs, &env)
			if len(env.Data) == 0 {
				break
			}
			for _, row := range env.Data {
				// Trade.ID()-equivalent uniqueness check.
				key := row.Source + ":" + row.TxHash + ":" +
					rowOpIndexString(row)
				if seenKeys[key] {
					t.Errorf("duplicate row across pages: %s", key)
				}
				seenKeys[key] = true
				collected = append(collected, row)
			}
			if env.Pagination == nil || env.Pagination.Next == "" {
				break
			}
			cursor = env.Pagination.Next
		}
		if len(collected) != 4 {
			t.Fatalf("drain returned %d rows, want 4", len(collected))
		}
		for i := 1; i < len(collected); i++ {
			if !collected[i-1].Timestamp.Before(collected[i].Timestamp) {
				t.Errorf("drain not chronological at i=%d", i)
			}
		}
	})

	t.Run("/v1/history empty window → empty array", func(t *testing.T) {
		// Window before all seeded trades → 0 rows.
		emptyFrom := t0.Add(-2 * time.Hour).Format(time.RFC3339)
		emptyTo := t0.Add(-1 * time.Hour).Format(time.RFC3339)
		var env struct {
			Data []v1.TradeRow `json:"data"`
		}
		getJSON(t, ts.URL+"/v1/history?"+pairQS+"&from="+emptyFrom+"&to="+emptyTo, &env)
		if len(env.Data) != 0 {
			t.Errorf("empty window returned %d rows", len(env.Data))
		}
		if env.Data == nil {
			t.Error("empty result must be [] not null")
		}
	})

	// Separate insert + drain for the "multiple trades share
	// (ts, ledger)" case. The previous drain test had trades at
	// distinct (ts, ledger) so the (ts, ledger)-only cursor would
	// pass — but real pagination across high-volume ledgers needs
	// the full-PK tiebreak. Seed a mini-cluster and prove no row
	// is dropped when the page break falls mid-cluster.
	t.Run("/v1/history cursor tiebreak — same (ts, ledger)", func(t *testing.T) {
		sharedTS := t0.Add(45 * time.Minute)
		for nonce := 10; nonce < 13; nonce++ {
			tr := mkAPITrade(nonce, sharedTS, pair, 1_000_000_000, 12_000_000)
			// Force same Ledger so the (ts, ledger) pair is shared.
			tr.Ledger = 60_000_000
			if err := store.InsertTrade(ctx, tr); err != nil {
				t.Fatalf("InsertTrade tiebreak trade %d: %v", nonce, err)
			}
		}

		// Narrow window + limit=1 → three separate pages.
		from := sharedTS.Add(-time.Second).Format(time.RFC3339)
		to := sharedTS.Add(time.Second).Format(time.RFC3339)
		seenTxs := map[string]bool{}
		cursor := ""
		for page := 0; page < 5; page++ {
			qs := pairQS + "&from=" + from + "&to=" + to + "&limit=1"
			if cursor != "" {
				qs += "&cursor=" + cursor
			}
			var env struct {
				Data       []v1.TradeRow `json:"data"`
				Pagination *struct {
					Next string `json:"next"`
				} `json:"pagination"`
			}
			getJSON(t, ts.URL+"/v1/history?"+qs, &env)
			if len(env.Data) == 0 {
				break
			}
			for _, row := range env.Data {
				if seenTxs[row.TxHash] {
					t.Errorf("duplicate tx_hash across pages: %s", row.TxHash)
				}
				seenTxs[row.TxHash] = true
			}
			if env.Pagination == nil || env.Pagination.Next == "" {
				break
			}
			cursor = env.Pagination.Next
		}
		if len(seenTxs) != 3 {
			t.Errorf("drain saw %d unique trades, want 3 — PK tiebreak likely broken", len(seenTxs))
		}
	})

	t.Run("/v1/vwap empty window → 404", func(t *testing.T) {
		emptyFrom := t0.Add(-2 * time.Hour).Format(time.RFC3339)
		emptyTo := t0.Add(-1 * time.Hour).Format(time.RFC3339)
		resp, err := http.Get(ts.URL + "/v1/vwap?" + pairQS + "&from=" + emptyFrom + "&to=" + emptyTo)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}

// TestAPI_Readyz stands up the same server with a real Timescale
// ReadyChecker and asserts /v1/readyz reports `ok` + the check
// round-trip covers the production Ping path. Before: readyz was
// only unit-tested with stubs — a regression in
// timescale.Store.DB().PingContext (e.g. a driver swap) wouldn't
// be caught here.
func TestAPI_Readyz(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	srv := v1.New(v1.Options{
		ReadyChecks: []v1.ReadyChecker{pgReadyChecker{s: store}},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v1/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("readyz status = %d, want 200", resp.StatusCode)
	}

	var env struct {
		Data struct {
			Status string `json:"status"`
			Checks []struct {
				Name string `json:"name"`
				OK   bool   `json:"ok"`
			} `json:"checks"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	if env.Data.Status != "ok" {
		t.Errorf("status = %q, want ok; env=%+v", env.Data.Status, env.Data)
	}
	found := false
	for _, ch := range env.Data.Checks {
		if ch.Name == "postgres" {
			found = true
			if !ch.OK {
				t.Errorf("postgres check reported not-OK against a live container")
			}
		}
	}
	if !found {
		t.Errorf("postgres check not in readyz response: %+v", env.Data.Checks)
	}
}

// pgReadyChecker mirrors cmd/stellarindex-api/main.go's
// storeChecker so the readyz integration exercises the exact Ping
// path production uses.
type pgReadyChecker struct{ s *timescale.Store }

func (c pgReadyChecker) Name() string                   { return "postgres" }
func (c pgReadyChecker) Critical() bool                 { return true }
func (c pgReadyChecker) Ping(ctx context.Context) error { return c.s.DB().PingContext(ctx) }

// TestAPI_OracleLatest stands up the v1 handler against a real
// Timescale with seeded oracle updates and walks the full path:
// InsertOracleUpdate → DISTINCT ON SQL → canonical parse round-
// trip → oracleReadingFrom rendering. Covers the behaviour the
// unit tests can only stub.
func TestAPI_OracleLatest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Seed two observations of XLM/USDC: reflector-dex older,
	// reflector-cex newer. /v1/oracle/latest returns the most-
	// recent reading per source — so both show up.
	usdc, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	price, _ := new(big.Int).SetString("12420000000000", 10)
	ts := time.Now().UTC().Truncate(time.Second)

	seeds := []c.OracleUpdate{
		{
			Source:    "reflector-dex",
			Ledger:    52_430_001,
			TxHash:    "1111111111111111111111111111111111111111111111111111111111111111",
			OpIndex:   0,
			Timestamp: ts.Add(-1 * time.Minute),
			Asset:     c.NativeAsset(), Quote: usdc,
			Price: c.NewAmount(price), Decimals: 14,
		},
		{
			Source:    "reflector-cex",
			Ledger:    52_430_002,
			TxHash:    "2222222222222222222222222222222222222222222222222222222222222222",
			OpIndex:   0,
			Timestamp: ts,
			Asset:     c.NativeAsset(), Quote: usdc,
			Price: c.NewAmount(price), Decimals: 14,
		},
	}
	for _, u := range seeds {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("InsertOracleUpdate: %v", err)
		}
	}

	srv := v1.New(v1.Options{Oracle: oracleAdapter{s: store}})
	hts := httptest.NewServer(srv.Handler())
	t.Cleanup(hts.Close)

	// No source filter — expect both sources back.
	var env struct {
		Data []v1.OracleReading `json:"data"`
	}
	getJSON(t, hts.URL+"/v1/oracle/latest?asset=native", &env)
	if len(env.Data) != 2 {
		t.Fatalf("got %d readings, want 2 (dex + cex)", len(env.Data))
	}
	gotSources := map[string]bool{}
	for _, r := range env.Data {
		gotSources[r.Source] = true
		if r.Price != "0.12420000000000" {
			t.Errorf("source %q price = %q, want 0.12420000000000",
				r.Source, r.Price)
		}
	}
	if !gotSources["reflector-dex"] || !gotSources["reflector-cex"] {
		t.Errorf("missing sources in response: %v", gotSources)
	}

	// With source filter — exactly one reading back.
	env = struct {
		Data []v1.OracleReading `json:"data"`
	}{}
	getJSON(t, hts.URL+"/v1/oracle/latest?asset=native&source=reflector-cex", &env)
	if len(env.Data) != 1 || env.Data[0].Source != "reflector-cex" {
		t.Fatalf("filtered response = %+v", env.Data)
	}
}

// TestStorage_LatestAggregatorPricesForPair exercises the
// reader the `aggregator_avg` price-authority tier consumes.
// Seeds three observations against (XLM, fiat:USD):
// coingecko + coinmarketcap (both aggregator-class) and
// reflector-cex (oracle-class). Asks the reader for aggregator
// sources only and verifies (a) it returns one row per requested
// aggregator source, (b) reflector is excluded, (c) the rows are
// the most-recent observation per source.
func TestStorage_LatestAggregatorPricesForPair(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usd, _ := c.NewFiatAsset("USD")
	price, _ := new(big.Int).SetString("17000000", 10)
	ts := time.Now().UTC().Truncate(time.Second)

	seeds := []c.OracleUpdate{
		// CG older, CG newer — DISTINCT ON (source) must return the newer.
		{
			Source: "coingecko", Ledger: 0,
			TxHash:  "1111111111111111111111111111111111111111111111111111111111111111",
			OpIndex: 0, Timestamp: ts.Add(-2 * time.Minute),
			Asset: c.NativeAsset(), Quote: usd,
			Price: c.NewAmount(price), Decimals: 8,
		},
		{
			Source: "coingecko", Ledger: 0,
			TxHash:  "2222222222222222222222222222222222222222222222222222222222222222",
			OpIndex: 0, Timestamp: ts,
			Asset: c.NativeAsset(), Quote: usd,
			Price: c.NewAmount(price), Decimals: 8,
		},
		// CMC single observation.
		{
			Source: "coinmarketcap", Ledger: 0,
			TxHash:  "3333333333333333333333333333333333333333333333333333333333333333",
			OpIndex: 0, Timestamp: ts.Add(-30 * time.Second),
			Asset: c.NativeAsset(), Quote: usd,
			Price: c.NewAmount(price), Decimals: 8,
		},
		// Oracle-class source — must NOT appear in the filtered result.
		{
			Source: "reflector-cex", Ledger: 52_000_000,
			TxHash:  "4444444444444444444444444444444444444444444444444444444444444444",
			OpIndex: 0, Timestamp: ts,
			Asset: c.NativeAsset(), Quote: usd,
			Price: c.NewAmount(price), Decimals: 14,
		},
	}
	for _, u := range seeds {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("InsertOracleUpdate: %v", err)
		}
	}

	got, err := store.LatestAggregatorPricesForPair(ctx,
		c.NativeAsset(), usd, []string{"coingecko", "coinmarketcap"})
	if err != nil {
		t.Fatalf("LatestAggregatorPricesForPair: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (one per aggregator)", len(got))
	}
	bySource := map[string]c.OracleUpdate{}
	for _, u := range got {
		bySource[u.Source] = u
	}
	if _, ok := bySource["coingecko"]; !ok {
		t.Error("coingecko missing from result")
	}
	if _, ok := bySource["coinmarketcap"]; !ok {
		t.Error("coinmarketcap missing from result")
	}
	if _, leaked := bySource["reflector-cex"]; leaked {
		t.Error("reflector-cex leaked into aggregator-only result")
	}
	// CG must be the *newer* observation (DISTINCT ON discipline).
	if cg := bySource["coingecko"]; !cg.Timestamp.Equal(ts) {
		t.Errorf("coingecko timestamp = %v, want %v (the newer seed)", cg.Timestamp, ts)
	}

	// Empty source list = (nil, nil) — caller's "no aggregators configured" case.
	got, err = store.LatestAggregatorPricesForPair(ctx, c.NativeAsset(), usd, nil)
	if err != nil {
		t.Fatalf("empty-source-list: unexpected err %v", err)
	}
	if got != nil {
		t.Errorf("empty source list returned %d rows; want nil", len(got))
	}
}

type oracleAdapter struct{ s *timescale.Store }

func (a oracleAdapter) LatestOracleUpdatesForAsset(ctx context.Context, asset c.Asset, sourceFilter string) ([]c.OracleUpdate, error) {
	return a.s.LatestOracleUpdatesForAsset(ctx, asset, sourceFilter)
}

func (a oracleAdapter) LatestOracleUpdatesForAssets(ctx context.Context, assets []c.Asset, sourceFilter string) ([]c.OracleUpdate, error) {
	return a.s.LatestOracleUpdatesForAssets(ctx, assets, sourceFilter)
}

func (a oracleAdapter) LatestOracleStreams(ctx context.Context) ([]c.OracleUpdate, error) {
	return a.s.LatestOracleStreams(ctx)
}

// ─── Adapters + helpers ───────────────────────────────────────────

// apiHistoryAdapter mirrors cmd/stellarindex-api/main.go's
// storeHistoryReader so the integration test exercises the same
// code path production does.
type apiHistoryAdapter struct{ s *timescale.Store }

func (r apiHistoryAdapter) TradesInRange(ctx context.Context, pair c.Pair, from, to time.Time, limit int) ([]c.Trade, error) {
	return r.s.TradesInRange(ctx, pair, from, to, limit)
}

func (r apiHistoryAdapter) TradesInRangeAfter(ctx context.Context, pair c.Pair, from, to, afterTs time.Time, afterLedger uint32, afterTxHash, afterSource string, afterOpIndex uint32, limit int) ([]c.Trade, error) {
	return r.s.TradesInRangeAfter(ctx, pair, from, to, afterTs, afterLedger, afterTxHash, afterSource, afterOpIndex, limit)
}

func (r apiHistoryAdapter) LatestTradePerSource(ctx context.Context, pair c.Pair, sourceFilter string) ([]c.Trade, error) {
	return r.s.LatestTradePerSource(ctx, pair, sourceFilter)
}

func (r apiHistoryAdapter) HistoryPoints(ctx context.Context, pair c.Pair, granularity string, limit int) ([]v1.HistoryPoint, error) {
	g := timescale.HistoryGranularity(granularity)
	if err := g.Validate(); err != nil {
		return nil, v1.ErrUnknownGranularity
	}
	rows, err := r.s.HistoryPoints(ctx, pair, g, limit)
	if err != nil {
		return nil, err
	}
	out := make([]v1.HistoryPoint, len(rows))
	for i, row := range rows {
		out[i] = v1.HistoryPoint{Bucket: row.Bucket, VWAP: row.VWAP, VolumeUSD: row.VolumeUSD}
	}
	return out, nil
}

func (r apiHistoryAdapter) HistoryPointsInRange(ctx context.Context, pair c.Pair, granularity string, from, to time.Time, limit int) ([]v1.HistoryPoint, error) {
	g := timescale.HistoryGranularity(granularity)
	if err := g.Validate(); err != nil {
		return nil, v1.ErrUnknownGranularity
	}
	rows, err := r.s.HistoryPointsInRange(ctx, pair, g, from, to, limit)
	if err != nil {
		return nil, err
	}
	out := make([]v1.HistoryPoint, len(rows))
	for i, row := range rows {
		out[i] = v1.HistoryPoint{Bucket: row.Bucket, VWAP: row.VWAP, VolumeUSD: row.VolumeUSD}
	}
	return out, nil
}

// TWAPPointsInRange is required by v1.HistoryReader (BACKLOG #37
// twap CAGG). Adapts [timescale.Store.TWAPPointsInRange]; only 1h/1d
// are backed by a TWAP CAGG (migration 0081).
func (r apiHistoryAdapter) TWAPPointsInRange(ctx context.Context, pair c.Pair, granularity string, from, to time.Time, limit int) ([]v1.HistoryPoint, error) {
	g := timescale.HistoryGranularity(granularity)
	if !timescale.TWAPGranularitySupported(g) {
		return nil, v1.ErrUnknownGranularity
	}
	rows, err := r.s.TWAPPointsInRange(ctx, pair, g, from, to, limit)
	if err != nil {
		return nil, err
	}
	out := make([]v1.HistoryPoint, len(rows))
	for i, row := range rows {
		out[i] = v1.HistoryPoint{Bucket: row.Bucket, VWAP: row.VWAP, VolumeUSD: row.VolumeUSD}
	}
	return out, nil
}

// OHLCSeries is required by v1.HistoryReader. The
// integration test for /v1/history doesn't exercise this path; the
// stub returns an empty series so the adapter implements the full
// interface without dragging in the production-side OHLCSeries
// re-bucketing logic.
func (r apiHistoryAdapter) OHLCSeries(_ context.Context, _ c.Pair, _ string, _, _ time.Time, _ int) ([]v1.OHLCSeriesBar, error) {
	return nil, nil
}

type apiMarketsAdapter struct{ s *timescale.Store }

func (r apiMarketsAdapter) DistinctPairsExt(ctx context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	rows, next, err := r.s.DistinctPairsExt(ctx, cursor, limit, order)
	if err != nil {
		return nil, "", err
	}
	out := make([]v1.Market, len(rows))
	for i, m := range rows {
		out[i] = v1.Market{
			Base:          m.Pair.Base.String(),
			Quote:         m.Pair.Quote.String(),
			LastTradeAt:   v1.WireTime(m.LastTradeAt),
			BucketCloseAt: v1.WireTime(m.BucketCloseAt),
			TradeCount24h: m.TradeCount24h,
			Volume24hUSD:  m.Volume24hUSD,
		}
	}
	return out, next, nil
}

func (r apiMarketsAdapter) FirstTradeBatch(ctx context.Context, pairs [][2]string) (map[string]time.Time, error) {
	return r.s.FirstTradeBatch(ctx, pairs)
}

func (r apiMarketsAdapter) PairMarket(ctx context.Context, base, quote c.Asset) (v1.Market, bool, error) {
	m, ok, err := r.s.PairMarket(ctx, base, quote)
	if err != nil || !ok {
		return v1.Market{}, ok, err
	}
	return v1.Market{
		Base:          m.Pair.Base.String(),
		Quote:         m.Pair.Quote.String(),
		LastTradeAt:   v1.WireTime(m.LastTradeAt),
		BucketCloseAt: v1.WireTime(m.BucketCloseAt),
		TradeCount24h: m.TradeCount24h,
		Volume24hUSD:  m.Volume24hUSD,
	}, true, nil
}

func (r apiMarketsAdapter) SourceMarkets(ctx context.Context, source, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	rows, next, err := r.s.SourceMarkets(ctx, source, cursor, limit, order)
	if err != nil {
		return nil, "", err
	}
	out := make([]v1.Market, len(rows))
	for i, m := range rows {
		out[i] = v1.Market{
			Base:          m.Pair.Base.String(),
			Quote:         m.Pair.Quote.String(),
			LastTradeAt:   v1.WireTime(m.LastTradeAt),
			BucketCloseAt: v1.WireTime(m.BucketCloseAt),
			TradeCount24h: m.TradeCount24h,
			Volume24hUSD:  m.Volume24hUSD,
		}
	}
	return out, next, nil
}

func (r apiMarketsAdapter) AssetMarkets(ctx context.Context, asset, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	rows, next, err := r.s.AssetMarkets(ctx, asset, cursor, limit, order)
	if err != nil {
		return nil, "", err
	}
	out := make([]v1.Market, len(rows))
	for i, m := range rows {
		out[i] = v1.Market{
			Base:          m.Pair.Base.String(),
			Quote:         m.Pair.Quote.String(),
			LastTradeAt:   v1.WireTime(m.LastTradeAt),
			BucketCloseAt: v1.WireTime(m.BucketCloseAt),
			TradeCount24h: m.TradeCount24h,
			Volume24hUSD:  m.Volume24hUSD,
		}
	}
	return out, next, nil
}

func (r apiMarketsAdapter) GetPairsVolumeHistory24hBatch(ctx context.Context, pairs [][2]string) (map[string][]timescale.PairVolumePoint, error) {
	return r.s.GetPairsVolumeHistory24hBatch(ctx, pairs)
}

func (r apiMarketsAdapter) AllPools(ctx context.Context, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Pool, string, error) {
	rows, next, err := r.s.AllPools(ctx, filter, cursor, limit, order)
	if err != nil {
		return nil, "", err
	}
	out := make([]v1.Pool, len(rows))
	for i, p := range rows {
		out[i] = v1.Pool{
			Source:        p.Source,
			Base:          p.Pair.Base.String(),
			Quote:         p.Pair.Quote.String(),
			LastTradeAt:   v1.WireTime(p.LastTradeAt),
			TradeCount24h: p.TradeCount24h,
			Volume24hUSD:  p.Volume24hUSD,
		}
	}
	return out, next, nil
}

// mkAPITrade builds a Trade with a unique TxHash per (ledger, nonce).
// Reuses the integration-test hex-encoding trick from
// pg_trades_test.go — keeps trade IDs distinct so the primary key
// doesn't collide.
func mkAPITrade(nonce int, ts time.Time, pair c.Pair, base, quote int64) c.Trade {
	h := make([]byte, 64)
	for i := range h {
		h[i] = '0'
	}
	const hex = "0123456789abcdef"
	h[62] = hex[(nonce>>4)&0xf]
	h[63] = hex[nonce&0xf]

	return c.Trade{
		Source:      "integ-api",
		Ledger:      uint32(50_000_000 + nonce),
		TxHash:      string(h),
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  c.NewAmount(big.NewInt(base)),
		QuoteAmount: c.NewAmount(big.NewInt(quote)),
	}
}

// rowOpIndexString gives a deterministic key component for the
// cursor-drain dedup check. TradeRow.OpIndex is uint32; we just
// want a stable string form.
func rowOpIndexString(r v1.TradeRow) string {
	return fmt.Sprintf("%d", r.OpIndex)
}

// getJSON fetches URL and decodes the response body into out. The
// body is always the Envelope shape our API serves (`{data: ...}`).
func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %d, body: %s", url, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode %s: %v (body: %s)", url, err, body)
	}
}
