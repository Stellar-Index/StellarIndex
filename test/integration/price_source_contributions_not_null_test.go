//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func migrateUpToErr(dsn string, version uint) error {
	_, thisFile, _, _ := runtime.Caller(0)
	m, err := migrate.New("file://"+filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations"), dsn)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	return m.Migrate(version)
}

func openContributionsDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func execAll(t *testing.T, ctx context.Context, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

func scalarInt(t *testing.T, ctx context.Context, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

// TestPriceSourceContributions_Migration0207 pins the release-N+2 step
// against real TimescaleDB, with the legacy chunks COMPRESSED:
//
//   - NULL-window rows are removed (whole legacy chunks dropped, the chunk
//     straddling the cut-over row-deleted) and every windowed row survives;
//   - window_seconds is NOT NULL and CHECK (> 0), the 0026 key is gone, so
//     a 5m row on a 1h row's bucket is accepted;
//   - down restores the nullable column and the 0026 key.
func TestPriceSourceContributions_Migration0207(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	if err := migrateUpToErr(dsn, 169); err != nil {
		t.Fatalf("migrate to 169: %v", err)
	}
	db := openContributionsDB(t, dsn)

	const ins = `INSERT INTO price_source_contributions (asset_id, quote_id, window_seconds, bucket, source, weight, trade_count) VALUES `
	execAll(t, ctx, db,
		// Two whole legacy chunks and a legacy row in the straddling chunk.
		ins+`('crypto:BTC','fiat:USD',NULL,TIMESTAMPTZ '2025-01-15 00:00:00Z','binance',1,1)`,
		ins+`('crypto:BTC','fiat:USD',NULL,TIMESTAMPTZ '2025-02-15 00:00:00Z','binance',1,1)`,
		ins+`('crypto:BTC','fiat:USD',NULL,TIMESTAMPTZ '2025-03-10 00:00:00Z','binance',1,1)`,
		ins+`('crypto:BTC','fiat:USD',300,TIMESTAMPTZ '2025-03-10 00:30:00Z','binance',1,1)`,
		ins+`('crypto:BTC','fiat:USD',3600,TIMESTAMPTZ '2025-03-10 00:30:01Z','binance',1,1)`,
		ins+`('crypto:BTC','fiat:USD',300,TIMESTAMPTZ '2025-04-01 00:00:00Z','binance',1,1)`,
	)
	chunksBefore := scalarInt(t, ctx, db, `SELECT count(*) FROM show_chunks('price_source_contributions')`)
	if compressed := scalarInt(t, ctx, db, `
		SELECT count(*) FROM (SELECT compress_chunk(c) FROM show_chunks('price_source_contributions') c) s`); compressed < 3 {
		t.Fatalf("compressed %d chunks, want >= 3 — the compressed-chunk claim would be vacuous", compressed)
	}

	if err := migrateUpToErr(dsn, 207); err != nil {
		t.Fatalf("migrate to 200: %v", err)
	}

	if n := scalarInt(t, ctx, db, `SELECT count(*) FROM price_source_contributions`); n != 3 {
		t.Errorf("rows after 0207 = %d, want the 3 windowed rows", n)
	}
	if n := scalarInt(t, ctx, db, `SELECT count(*) FROM price_source_contributions WHERE window_seconds IS NOT NULL`); n != 3 {
		t.Errorf("windowed rows after 0207 = %d, want 3", n)
	}
	if after := scalarInt(t, ctx, db, `SELECT count(*) FROM show_chunks('price_source_contributions')`); after >= chunksBefore {
		t.Errorf("chunks %d -> %d, want the all-legacy chunks dropped", chunksBefore, after)
	}
	if n := scalarInt(t, ctx, db, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'price_source_contributions' AND column_name = 'window_seconds' AND is_nullable = 'NO'`); n != 1 {
		t.Error("window_seconds is still nullable")
	}
	if n := scalarInt(t, ctx, db, `
		SELECT count(*) FROM pg_constraint
		 WHERE conrelid = 'price_source_contributions'::regclass AND contype = 'p'`); n != 0 {
		t.Errorf("%d primary keys remain, want the 0026 key dropped", n)
	}

	// The 0026 key refused this; the window key accepts it.
	if _, err := db.ExecContext(ctx, ins+`('crypto:BTC','fiat:USD',86400,TIMESTAMPTZ '2025-03-10 00:30:00Z','binance',1,1)`); err != nil {
		t.Errorf("a second window on an occupied bucket was refused: %v", err)
	}
	if _, err := db.ExecContext(ctx, ins+`('crypto:ETH','fiat:USD',NULL,now(),'kraken',1,1)`); err == nil {
		t.Error("a NULL-window row was accepted after 0207")
	}
	if _, err := db.ExecContext(ctx, ins+`('crypto:ETH','fiat:USD',0,now(),'kraken',1,1)`); err == nil {
		t.Error("a zero-window row was accepted after 0207")
	}

	// Down cannot restore the 0026 key over rows that collide on it.
	execAll(t, ctx, db, `DELETE FROM price_source_contributions WHERE window_seconds = 86400`)
	if err := migrateUpToErr(dsn, 205); err != nil {
		t.Fatalf("migrate down to 199: %v", err)
	}
	if n := scalarInt(t, ctx, db, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'price_source_contributions' AND column_name = 'window_seconds' AND is_nullable = 'YES'`); n != 1 {
		t.Error("after 0207 down window_seconds is not nullable again")
	}
}

// TestPriceSourceContributions_Migration0207_RefusesActiveLegacyWriter pins
// the guard: NULL rows with no windowed row after them mean a pre-0169
// writer is (or was last) active, and nothing may be deleted.
func TestPriceSourceContributions_Migration0207_RefusesActiveLegacyWriter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	if err := migrateUpToErr(dsn, 169); err != nil {
		t.Fatalf("migrate to 169: %v", err)
	}
	db := openContributionsDB(t, dsn)
	execAll(t, ctx, db, `
		INSERT INTO price_source_contributions (asset_id, quote_id, bucket, source, weight, trade_count)
		VALUES ('crypto:BTC','fiat:USD', now(), 'binance', 1, 1)`)

	err := migrateUpToErr(dsn, 207)
	if err == nil || errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("0207 applied over live NULL-window rows: err = %v", err)
	}
	if n := scalarInt(t, ctx, db, `SELECT count(*) FROM price_source_contributions`); n != 1 {
		t.Errorf("rows after refused 0207 = %d, want 1 (nothing deleted)", n)
	}
}
