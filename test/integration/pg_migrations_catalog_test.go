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
	// Discovered, not listed, so a trades-derived cagg added later without the interval fails here.
	// Direct: raw hypertable is trades. Hierarchical: raw hypertable is a derived cagg's
	// materialization hypertable. Joined: the definition names trades.
	const q = `
		WITH RECURSIVE derived AS (
		    SELECT ca.mat_hypertable_id
		      FROM _timescaledb_catalog.continuous_agg ca
		      JOIN _timescaledb_catalog.hypertable h ON h.id = ca.raw_hypertable_id
		     WHERE h.schema_name = 'public' AND h.table_name = 'trades'
		    UNION
		    SELECT ca.mat_hypertable_id
		      FROM _timescaledb_catalog.continuous_agg ca
		     WHERE ca.user_view_schema = 'public'
		       AND pg_get_viewdef(format('%I.%I', ca.user_view_schema, ca.user_view_name)::regclass) ~ '\mtrades\M'
		    UNION
		    SELECT ca.mat_hypertable_id
		      FROM _timescaledb_catalog.continuous_agg ca
		      JOIN derived d ON ca.raw_hypertable_id = d.mat_hypertable_id
		)
		SELECT ca.user_view_name, dim.time_interval::text
		  FROM derived
		  JOIN _timescaledb_catalog.continuous_agg ca ON ca.mat_hypertable_id = derived.mat_hypertable_id
		  JOIN _timescaledb_catalog.hypertable mh ON mh.id = ca.mat_hypertable_id
		  JOIN timescaledb_information.dimensions dim
		    ON dim.hypertable_schema = mh.schema_name AND dim.hypertable_name = mh.table_name
		 WHERE dim.dimension_type = 'Time'
		 ORDER BY 1`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("discover trades-derived continuous aggregates: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	var names, wrong []string
	for rows.Next() {
		var v, w string
		if err := rows.Scan(&v, &w); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[v] = true
		names = append(names, v)
		if w != "7 days" {
			wrong = append(wrong, v+" ("+w+")")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	t.Logf("trades-derived continuous aggregates (%d): %s", len(names), strings.Join(names, ", "))
	for _, v := range []string{"prices_1m", "twap_1d", "trade_sums_1m", "dex_volume_by_pair_1d", "source_volume_1h"} {
		if !seen[v] {
			t.Errorf("discovery missed %s, a known trades-derived continuous aggregate; the query no longer matches the catalog", v)
		}
	}
	for _, v := range wrong {
		t.Errorf("materialization chunk interval of %s, want \"7 days\": set it in a migration", v)
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

// 0191 down must refuse while a zero-leg row exists, leaving the CHECKs absent and the rows intact.
func TestTrades0191DownRefusesZeroLegRows(t *testing.T) {
	ctx, db := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset, base_amount, quote_amount)
		VALUES ('t', 1, 'z1', 0, now(), 'native', 'native', 0, 2),
		       ('t', 2, 'z2', 0, now(), 'native', 'native', 3, 4)`); err != nil {
		t.Fatalf("insert trades: %v", err)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	body, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations", "0191_trades_drop_amount_checks.down.sql"))
	if err != nil {
		t.Fatalf("read 0191 down: %v", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	_, downErr := conn.ExecContext(ctx, string(body))
	// The refused body leaves its explicit transaction aborted on this session.
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatalf("rollback after refused down: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close conn: %v", err)
	}
	if downErr == nil || !strings.Contains(downErr.Error(), "zero or negative leg") {
		t.Fatalf("0191 down over a zero-leg row: err = %v, want the zero-or-negative-leg refusal", downErr)
	}

	var checks, rows int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_constraint
		 WHERE conrelid = 'trades'::regclass
		   AND conname IN ('trades_base_amount_check', 'trades_quote_amount_check')`).Scan(&checks); err != nil {
		t.Fatalf("count constraints: %v", err)
	}
	if checks != 0 {
		t.Errorf("refused 0191 down left %d amount CHECKs on trades, want 0", checks)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM trades WHERE tx_hash IN ('z1', 'z2')`).Scan(&rows); err != nil {
		t.Fatalf("count trades: %v", err)
	}
	if rows != 2 {
		t.Errorf("trades rows after refused 0191 down = %d, want 2 (the down must not delete)", rows)
	}
}
