//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func wantCheckViolation(t *testing.T, what string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Errorf("%s: err = %v, want SQLSTATE 23514", what, err)
	}
}

const insertWebhookSQL = `INSERT INTO customer_webhooks (account_id, name, url, secret_hash, signing_key_sealed, events)
		VALUES ($1, $2, $3, $4, $5, ARRAY['incident.sev1']) RETURNING id`

// TestMigration0235_RefusesDualKeyRow: a webhook holding a raw and a sealed current key under 0234 is not repaired by
// guessing; 0235 stops, names the row's id, and leaves both columns as they were.
func TestMigration0235_RefusesDualKeyRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 234)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct := insertRawAccount(t, ctx, db, "dual-key")

	raw, sealed := []byte("fixture-raw-key-beside-sealed"), []byte{1, 0xAA, 0xBB, 0xCC}
	var dual string
	if err := db.QueryRowContext(ctx, insertWebhookSQL, acct, "dual", "https://dual.example/hook", raw, sealed).Scan(&dual); err != nil {
		t.Fatalf("seed dual-key row under 0234: %v", err)
	}

	err = applyMigrationsUpToErr(dsn, 235)
	if err == nil || !strings.Contains(err.Error(), dual) {
		t.Fatalf("0235 over a dual-key row: err = %v, want a refusal naming id %s", err, dual)
	}
	if strings.Contains(err.Error(), string(raw)) {
		t.Errorf("0235's error carries key material: %v", err)
	}
	var gotRaw, gotSealed []byte
	if err := db.QueryRowContext(ctx, `SELECT secret_hash, signing_key_sealed FROM customer_webhooks WHERE id = $1`, dual).
		Scan(&gotRaw, &gotSealed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRaw, raw) || !bytes.Equal(gotSealed, sealed) {
		t.Errorf("refused row changed: secret_hash = %q, signing_key_sealed = %x; want both kept", gotRaw, gotSealed)
	}
}

// TestMigration0235_ForbidsDualKeyRow applies 0235 and 0236 to a clean table: a new dual-key or keyless webhook and a
// decimals value above 38 are refused.
func TestMigration0235_ForbidsDualKeyRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 234)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct := insertRawAccount(t, ctx, db, "one-key")
	sealed := []byte{1, 0xAA, 0xBB, 0xCC}
	if _, err := db.ExecContext(ctx, `INSERT INTO nonstandard_decimals_assets (asset, decimals, source, confirmed_at)
		VALUES ('CAAA', 18, 'soroswap', now())`); err != nil {
		t.Fatalf("seed nonstandard_decimals_assets: %v", err)
	}

	applyMigrations(t, dsn)

	insert := insertWebhookSQL
	var one string
	if err := db.QueryRowContext(ctx, insert, acct, "sealed", "https://sealed.example/hook", nil, sealed).Scan(&one); err != nil {
		t.Errorf("a sealed-only row refused: %v", err)
	}
	_, err = db.ExecContext(ctx, insert, acct, "dual2", "https://dual2.example/hook", []byte("fixture-raw"), sealed)
	wantCheckViolation(t, "a new dual-key row", err)
	_, err = db.ExecContext(ctx, insert, acct, "none", "https://none.example/hook", nil, nil)
	wantCheckViolation(t, "a keyless row", err)
	_, err = db.ExecContext(ctx, `INSERT INTO nonstandard_decimals_assets (asset, decimals, source, confirmed_at)
		VALUES ('CBBB', 39, 'soroswap', now())`)
	wantCheckViolation(t, "decimals 39", err)
	if _, err := db.ExecContext(ctx, `INSERT INTO nonstandard_decimals_assets (asset, decimals, source, confirmed_at)
		VALUES ('CCCC', 38, 'soroswap', now())`); err != nil {
		t.Errorf("decimals 38 refused: %v", err)
	}
}

// TestMigration0236_StopsOnAnOutOfRangeRow: a decimals value above 38 already stored is not guessed at; VALIDATE fails
// and the migration stops for an operator to correct the row.
func TestMigration0236_StopsOnAnOutOfRangeRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 235)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `INSERT INTO nonstandard_decimals_assets (asset, decimals, source, confirmed_at)
		VALUES ('CDDD', 180, 'soroswap', now())`); err != nil {
		t.Fatalf("seed decimals 180 under 0235: %v", err)
	}
	if err := applyMigrationsUpToErr(dsn, 236); err == nil {
		t.Fatal("0236 applied over a decimals=180 row; want VALIDATE to fail")
	}
}
