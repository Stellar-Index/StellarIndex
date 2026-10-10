//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/accounterasure"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

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
