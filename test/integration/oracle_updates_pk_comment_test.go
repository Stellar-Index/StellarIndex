//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// 0003's stored comment, verbatim; 0176's down must restore exactly this.
const oracleUpdatesComment0003 = `Every observed oracle publication, one row per (source, ledger, tx_hash, op_index). ` +
	`Hypertable partitioned on ts. See ADR-0006.`

// TestOracleUpdatesPKComment pins the catalog comment on oracle_updates
// after migration 0176 up and down (T094). 0003's comment claimed "one row
// per (source, ledger, tx_hash, op_index)" — the PRIMARY KEY also carries
// `ts`, so a decoder-ts-derivation fix that replays affected ledgers adds a
// second row for the same publication instead of replacing the first. The
// comment lives in pg_description, so only a real database can say what an
// operator reads through `\d+`.
func TestOracleUpdatesPKComment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	got := oracleUpdatesTableComment(t, ctx, db)
	for _, want := range []string{
		"NOT because it is part of the logical",
		"canonical.OracleUpdate.ID()",
		"DELETE the stale row",
		"(source, ledger, tx_hash, op_index)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("oracle_updates comment missing %q; got %q", want, got)
		}
	}
	if strings.Contains(got, "one row per (source, ledger, tx_hash, op_index)") {
		t.Errorf("oracle_updates comment still claims the PK is (source, ledger, tx_hash, "+
			"op_index) alone, hiding that `ts` is also part of it; got %q", got)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Migrate(175); err != nil {
		t.Fatalf("migrate down to 175: %v", err)
	}
	if got := oracleUpdatesTableComment(t, ctx, db); got != oracleUpdatesComment0003 {
		t.Errorf("0176 down did not restore 0003's comment verbatim:\n got %q\nwant %q", got, oracleUpdatesComment0003)
	}
}

func oracleUpdatesTableComment(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var c sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT obj_description('oracle_updates'::regclass, 'pg_class')`,
	).Scan(&c); err != nil {
		t.Fatalf("read oracle_updates comment: %v", err)
	}
	if !c.Valid {
		t.Fatal("oracle_updates has no catalog comment")
	}
	return c.String
}
