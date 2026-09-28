//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAccountErasureTrigger pins 0188's audit_log exception: only the
// exact erasure scrub of a closed account's own rows passes.
func TestAccountErasureTrigger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var acct, other uuid.UUID
	mustScan(t, ctx, db, &acct, `INSERT INTO accounts (name, slug, billing_email) VALUES ('a', 'trig', 'a@x.example') RETURNING id`)
	mustScan(t, ctx, db, &other, `INSERT INTO accounts (name, slug, billing_email) VALUES ('b', 'other', 'b@x.example') RETURNING id`)
	var userRow, staffRow, otherRow uuid.UUID
	mustScan(t, ctx, db, &userRow, `INSERT INTO audit_log (account_id, actor_kind, action, metadata, ip, user_agent)
		VALUES ($1, 'user', 'key.mint', '{"name":"k","scopes":["a"]}', '198.51.100.1', 'UA') RETURNING id`, acct)
	mustScan(t, ctx, db, &staffRow, `INSERT INTO audit_log (account_id, actor_kind, action, metadata, ip, user_agent)
		VALUES ($1, 'staff', 'admin.account.read', '{"account_slug":"trig","actor_key_id":"kid"}', '203.0.113.1', 'S') RETURNING id`, acct)
	mustScan(t, ctx, db, &otherRow, `INSERT INTO audit_log (account_id, actor_kind, action, metadata, ip)
		VALUES ($1, 'user', 'key.mint', '{"name":"k"}', '198.51.100.2') RETURNING id`, other)

	scrub := `UPDATE audit_log SET metadata = audit_log_erase_metadata(metadata), ip = NULL, user_agent = NULL WHERE id = $1`
	inTx := func(guc string, stmt string, args ...any) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if guc != "" {
			if _, err := tx.ExecContext(ctx, `SELECT set_config('stellarindex.erasing_account', $1, true)`, guc); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return err
		}
		return tx.Commit()
	}

	requireAppendOnlyRefusal(t, "scrub without the GUC", inTx("", scrub, userRow))
	requireAppendOnlyRefusal(t, "scrub of an active account", inTx(acct.String(), scrub, userRow))
	mustExec(t, ctx, db, `UPDATE accounts SET status = 'closed' WHERE id = $1`, acct)
	requireAppendOnlyRefusal(t, "scrub of another account's row", inTx(acct.String(), scrub, otherRow))
	requireAppendOnlyRefusal(t, "scrub that rewrites action",
		inTx(acct.String(), `UPDATE audit_log SET metadata = audit_log_erase_metadata(metadata), action = 'x' WHERE id = $1`, userRow))
	requireAppendOnlyRefusal(t, "metadata other than the erase function's",
		inTx(acct.String(), `UPDATE audit_log SET metadata = '{}' WHERE id = $1`, userRow))
	requireAppendOnlyRefusal(t, "nulling a staff address", inTx(acct.String(), scrub, staffRow))
	requireAppendOnlyRefusal(t, "delete under the GUC", inTx(acct.String(), `DELETE FROM audit_log WHERE id = $1`, userRow))

	if err := inTx(acct.String(), scrub, userRow); err != nil {
		t.Fatalf("permitted scrub refused: %v", err)
	}
	if err := inTx(acct.String(), `UPDATE audit_log SET metadata = audit_log_erase_metadata(metadata) WHERE id = $1`, staffRow); err != nil {
		t.Fatalf("permitted staff metadata scrub refused: %v", err)
	}
	var meta, staffMeta string
	var ip sql.NullString
	mustScan(t, ctx, db, &meta, `SELECT metadata::text FROM audit_log WHERE id = $1`, userRow)
	mustScan(t, ctx, db, &ip, `SELECT host(ip) FROM audit_log WHERE id = $1`, userRow)
	mustScan(t, ctx, db, &staffMeta, `SELECT metadata::text || host(ip) FROM audit_log WHERE id = $1`, staffRow)
	if meta != `{"scopes": ["a"]}` || ip.Valid {
		t.Errorf("user row after scrub = %s ip=%v, want name removed and ip NULL", meta, ip)
	}
	if staffMeta != `{"actor_key_id": "kid"}203.0.113.1` {
		t.Errorf("staff row after scrub = %s, want slug removed and address kept", staffMeta)
	}
}

func mustExec(t *testing.T, ctx context.Context, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}

func mustScan(t *testing.T, ctx context.Context, db *sql.DB, dst any, q string, args ...any) {
	t.Helper()
	if err := db.QueryRowContext(ctx, q, args...).Scan(dst); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}
