//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func openMigrated(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db
}

// A recreate sizes materialization chunks at 10x the source interval; every trades-derived view must stay at 7 days.
func TestCAGGMaterializationChunkInterval(t *testing.T) {
	ctx, db := openMigrated(t)
	const q = `
		SELECT d.time_interval::text
		  FROM timescaledb_information.continuous_aggregates ca
		  JOIN timescaledb_information.dimensions d
		    ON d.hypertable_schema = ca.materialization_hypertable_schema
		   AND d.hypertable_name = ca.materialization_hypertable_name
		 WHERE ca.view_schema = 'public' AND ca.view_name = $1 AND d.dimension_type = 'Time'`
	for _, v := range []string{
		"prices_1m", "prices_15m", "prices_1h", "prices_4h", "prices_1d", "prices_1w", "prices_1mo",
		"twap_1h", "twap_1d", "pools_per_source_1h", "trade_sums_1m",
	} {
		var w string
		if err := db.QueryRowContext(ctx, q, v).Scan(&w); err != nil {
			t.Fatalf("read %s materialization interval: %v", v, err)
		}
		if w != "7 days" {
			t.Errorf("%s materialization chunk interval = %q, want \"7 days\"", v, w)
		}
	}
}

// The stored comment must tell a \d+ reader that rows drive the price correction, not a decline.
func TestNonstandardDecimalsAssetsComment(t *testing.T) {
	ctx, db := openMigrated(t)
	var c string
	if err := db.QueryRowContext(ctx,
		`SELECT obj_description('nonstandard_decimals_assets'::regclass, 'pg_class')`).Scan(&c); err != nil {
		t.Fatalf("read comment: %v", err)
	}
	if !strings.Contains(c, "AdjustPrice") || strings.Contains(c, "declines") {
		t.Fatalf("nonstandard_decimals_assets comment still describes a decline guard: %q", c)
	}
}

// 0191 down must restore the > 0 CHECKs over compressed trades chunks, validating the compressed rows in place.
func TestTrades0191DownOverCompressedChunks(t *testing.T) {
	ctx, db := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset, base_amount, quote_amount)
		VALUES ('t', 1, 'a1', 0, now() - INTERVAL '60 days', 'native', 'native', 1, 2),
		       ('t', 2, 'a2', 0, now() - INTERVAL '30 days', 'native', 'native', 3, 4)`); err != nil {
		t.Fatalf("insert trades: %v", err)
	}
	var compressed int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM (SELECT compress_chunk(c) FROM show_chunks('trades') c) s`).Scan(&compressed); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if compressed == 0 {
		t.Fatal("no trades chunk compressed — the fixture no longer reproduces")
	}

	_, thisFile, _, _ := runtime.Caller(0)
	body, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations", "0191_trades_drop_amount_checks.down.sql"))
	if err != nil {
		t.Fatalf("read 0191 down: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("0191 down over compressed trades chunks: %v", err)
	}

	var still int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM timescaledb_information.chunks
		 WHERE hypertable_name = 'trades' AND is_compressed`).Scan(&still); err != nil {
		t.Fatalf("count compressed: %v", err)
	}
	if still != compressed {
		t.Errorf("compressed trades chunks after down = %d, want %d (down must not decompress)", still, compressed)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset, base_amount, quote_amount)
		VALUES ('t', 3, 'a3', 0, now(), 'native', 'native', 0, 1)`); err == nil {
		t.Error("zero base_amount accepted after 0191 down; the CHECK was not restored")
	}
}
