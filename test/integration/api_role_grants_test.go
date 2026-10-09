//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// apiWritableTables is the exact write set 0213 grants stellarindex_api.
var apiWritableTables = map[string]bool{
	"accounts": true, "api_keys": true, "api_usage_events": true, "audit_log": true,
	"customer_webhooks": true, "erased_account_slugs": true, "fx_fixings": true,
	"fx_quotes": true, "invites": true, "login_code_lockouts": true,
	"magic_link_tokens": true, "price_alerts": true, "sessions": true,
	"source_entry_counts": true, "status_notices": true, "usage_daily": true,
	"users": true, "webauthn_credentials": true, "webhook_deliveries": true,
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

	dsn := startTimescale(t, ctx)
	owner, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer owner.Close()
	applyMigrations(t, dsn)

	pair := ohlcDustPair{base: "GRNT-" + priceableIssuer, quote: "native"}
	t0 := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour)
	seed(t, owner, ctx, pair, []seedTrade{{off: 0, base: "1000", quote: "5000", usd: "1"}}, t0)
	compressTradesChunks(t, ctx, owner, 1)

	const pwd = "api-role-fixture" // gitleaks:allow — throwaway container password, not a credential
	mustExec(t, ctx, owner, `CREATE ROLE stellarindex_api LOGIN PASSWORD '`+pwd+`'`)
	mustExec(t, ctx, owner, `SELECT apply_api_role_grants()`)

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword("stellarindex_api", pwd)
	api, err := sql.Open("pgx", u.String())
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

	// A granted write draws its id from a default and succeeds.
	mustExec(t, ctx, api, `INSERT INTO audit_log (actor_kind, action) VALUES ('system', 'api-role-grant-test')`)

	for name, stmt := range map[string]string{
		"insert trades":            `INSERT INTO trades SELECT * FROM trades LIMIT 1`,
		"update trades":            `UPDATE trades SET usd_volume = 0`,
		"delete trades":            `DELETE FROM trades`,
		"delete ingestion_cursors": `DELETE FROM ingestion_cursors`,
		"update oracle_updates":    `UPDATE oracle_updates SET price = 0`,
		"delete asset_volume_24h":  `DELETE FROM asset_volume_24h`,
		"truncate accounts":        `TRUNCATE accounts CASCADE`,
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
		if !sel || ins != want || upd != want || del != want || trunc {
			t.Errorf("%s: select=%v insert=%v update=%v delete=%v truncate=%v, want select and writes=%v, no truncate",
				rel, sel, ins, upd, del, trunc, want)
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
}

func requireInsufficientPrivilege(t *testing.T, ctx context.Context, db *sql.DB, name, stmt string) {
	t.Helper()
	_, err := db.ExecContext(ctx, stmt)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("%s: err = %v, want 42501 insufficient_privilege", name, err)
	}
}
