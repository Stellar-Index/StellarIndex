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

// 0051's stored table comment, verbatim; 0182's down must restore exactly this.
const ledgerIngestLogComment0051 = `Substrate-continuity record (ADR-0033). One row per fully-` +
	`processed ledger, written post-persist. soroban_event_count / ` +
	`classic_trade_effect_count are LCM-derived checksums reconciled ` +
	`against soroban_events / trades. Contiguity + hash-chain over ` +
	`this table is Claim 1 of the completeness model.`

// TestLedgerIngestLogComment pins the catalog comments on ledger_ingest_log
// after migration 0182 up and down (GH #923): the indexer writes the row
// after ENQUEUE to the async sink, so `\d+` must not call it post-persist.
func TestLedgerIngestLogComment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	table, col := ledgerIngestLogComments(t, ctx, db)
	for name, got := range map[string]sql.NullString{"table": table, "persisted_at": col} {
		if !got.Valid {
			t.Fatalf("ledger_ingest_log %s has no catalog comment", name)
		}
		if !strings.Contains(strings.ToLower(got.String), "enqueued") || !strings.Contains(got.String, "census-backfill") {
			t.Errorf("%s comment does not name the enqueue/census-backfill writers; got %q", name, got.String)
		}
		if strings.Contains(got.String, "post-persist") {
			t.Errorf("%s comment still claims post-persist; got %q", name, got.String)
		}
	}
	if !strings.Contains(table.String, "NOT a persistence marker") {
		t.Errorf("table comment does not disclaim persistence; got %q", table.String)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Migrate(181); err != nil {
		t.Fatalf("migrate down to 181: %v", err)
	}
	table, col = ledgerIngestLogComments(t, ctx, db)
	if table.String != ledgerIngestLogComment0051 {
		t.Errorf("0182 down did not restore 0051's table comment verbatim:\n got %q\nwant %q", table.String, ledgerIngestLogComment0051)
	}
	if col.Valid {
		t.Errorf("0182 down left a persisted_at comment 0051 never set: %q", col.String)
	}
}

func ledgerIngestLogComments(t *testing.T, ctx context.Context, db *sql.DB) (table, col sql.NullString) {
	t.Helper()
	if err := db.QueryRowContext(ctx,
		`SELECT obj_description('ledger_ingest_log'::regclass, 'pg_class'),
		        col_description('ledger_ingest_log'::regclass, a.attnum)
		   FROM pg_attribute a
		  WHERE a.attrelid = 'ledger_ingest_log'::regclass AND a.attname = 'persisted_at'`,
	).Scan(&table, &col); err != nil {
		t.Fatalf("read ledger_ingest_log comments: %v", err)
	}
	return table, col
}
