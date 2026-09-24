//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPriceSourceContributions_Migration0169 pins GH #763 against real
// TimescaleDB:
//
//   - 0169 applies to a price_source_contributions hypertable that already
//     holds a COMPRESSED chunk written by the previous binary, and that
//     legacy row keeps window_seconds NULL (its window is unrecoverable);
//   - one tick's 5m, 1h and 24h breakdowns of a pair are three rows, each
//     carrying its own window, and "latest bucket per window" returns each
//     window's own weights — before 0169 the rows had no window and that
//     read returned whichever window ran last;
//   - while 0026's key survives, a second window on an occupied bucket is
//     REFUSED instead of silently overwriting the other window's row;
//   - the previous binary's exact INSERT … ON CONFLICT still executes
//     (migrations/README.md rule 9);
//   - a row with no window is refused and nothing is written;
//   - the retention policy exists and is NOT scheduled;
//   - 0169's down drops the column and the policy and keeps the rows.
func TestPriceSourceContributions_Migration0169(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 168)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seedAndCompressContributionChunk(t, ctx, db)

	applyMigrations(t, dsn)
	assertContributionChunkCompressed(t, ctx, db)

	var legacyWindow sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT window_seconds FROM price_source_contributions
		 WHERE bucket = TIMESTAMPTZ '2025-01-15 00:00:00Z'`).Scan(&legacyWindow); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if legacyWindow.Valid {
		t.Errorf("legacy row window_seconds = %d, want NULL", legacyWindow.Int64)
	}

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	bucket := time.Now().UTC().Truncate(time.Microsecond)
	assertThreeWindowsStayDistinct(t, ctx, db, store, bucket)
	assertOldBinaryInsertStillWorks(t, ctx, db, bucket)
	assertMissingWindowWritesNothing(t, ctx, db, store, bucket)
	assertRetentionPolicyDisabled(t, ctx, db)

	applyMigrationsUpTo(t, dsn, 168)
	assertDownDropsWindowAndPolicy(t, ctx, db)
}

func seedAndCompressContributionChunk(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO price_source_contributions (asset_id, quote_id, bucket, source, weight, trade_count)
		VALUES ('crypto:BTC', 'fiat:USD', TIMESTAMPTZ '2025-01-15 00:00:00Z', 'binance', 1, 4)`); err != nil {
		t.Fatalf("seed legacy contribution row: %v", err)
	}
	var compressed int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT compress_chunk(c) FROM show_chunks('price_source_contributions') c
		) s`).Scan(&compressed); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if compressed == 0 {
		t.Fatal("no price_source_contributions chunk compressed — the compressed-chunk claim would be vacuous")
	}
}

func assertContributionChunkCompressed(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM timescaledb_information.chunks
		 WHERE hypertable_name = 'price_source_contributions' AND is_compressed`).Scan(&n); err != nil {
		t.Fatalf("read chunk compression state: %v", err)
	}
	if n == 0 {
		t.Fatal("no compressed chunk after migrating — 0169 was NOT exercised against compressed data")
	}
}

func assertThreeWindowsStayDistinct(t *testing.T, ctx context.Context, db *sql.DB, store *timescale.Store, bucket time.Time) {
	t.Helper()
	// One tick, as the orchestrator writes it: the three windows in
	// order, each stamped a few microseconds after the last.
	ethRow := func(w time.Duration, at time.Time, weight float64) []timescale.PriceSourceContribution {
		return []timescale.PriceSourceContribution{{
			AssetID: "crypto:ETH", QuoteID: "fiat:USD", Window: w, Bucket: at,
			Source: "kraken", Weight: weight, TradeCount: 2,
		}}
	}
	for i, w := range []time.Duration{5 * time.Minute, time.Hour, 24 * time.Hour} {
		// A first write with a wrong weight, then the same (window, bucket)
		// again: the window-aware ON CONFLICT arm must update it in place.
		at := bucket.Add(time.Duration(i) * time.Microsecond)
		if err := store.InsertPriceSourceContributions(ctx, ethRow(w, at, 0.01)); err != nil {
			t.Fatalf("insert window %s: %v", w, err)
		}
		if err := store.InsertPriceSourceContributions(ctx, ethRow(w, at, float64(i+1)/4)); err != nil {
			t.Fatalf("upsert window %s: %v", w, err)
		}
	}
	assertLatestPerWindow(t, ctx, db, map[int64]float64{300: 0.25, 3600: 0.5, 86400: 0.75})

	// While 0026's key survives (release N), a second window on an
	// occupied bucket must FAIL, not overwrite the other window's row.
	if err := store.InsertPriceSourceContributions(ctx, ethRow(5*time.Minute, bucket.Add(time.Microsecond), 0.9)); err == nil {
		t.Error("a 5m row on the 1h row's bucket was accepted; it must be refused while the 0026 key stands")
	}
	assertLatestPerWindow(t, ctx, db, map[int64]float64{300: 0.25, 3600: 0.5, 86400: 0.75})
}

// assertLatestPerWindow runs the read the table documents — the latest
// bucket per (asset_id, quote_id, window_seconds) — for crypto:ETH.
func assertLatestPerWindow(t *testing.T, ctx context.Context, db *sql.DB, want map[int64]float64) {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT ON (window_seconds) window_seconds, weight::float8
		  FROM price_source_contributions
		 WHERE asset_id = 'crypto:ETH' AND quote_id = 'fiat:USD' AND source = 'kraken'
		   AND window_seconds IS NOT NULL
		 ORDER BY window_seconds, bucket DESC`)
	if err != nil {
		t.Fatalf("read windows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got := map[int64]float64{}
	for rows.Next() {
		var ws int64
		var weight float64
		if err := rows.Scan(&ws, &weight); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[ws] = weight
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("latest row per window = %v, want %v", got, want)
	}
	for ws, w := range want {
		if got[ws] != w {
			t.Errorf("window_seconds=%d weight = %v, want %v", ws, got[ws], w)
		}
	}
}

// The exact statement the previous binary issues. It names the 0026 key in
// ON CONFLICT, so it only keeps working while that key survives.
func assertOldBinaryInsertStillWorks(t *testing.T, ctx context.Context, db *sql.DB, bucket time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO price_source_contributions (
		    asset_id, quote_id, bucket, source,
		    weight, volume_usd, trade_count
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (asset_id, quote_id, bucket, source) DO UPDATE SET
		    weight       = EXCLUDED.weight,
		    volume_usd   = EXCLUDED.volume_usd,
		    trade_count  = EXCLUDED.trade_count`,
		"crypto:XRP", "fiat:USD", bucket, "coinbase", 1.0, nil, 1); err != nil {
		t.Errorf("previous binary's INSERT fails against 0169 (rule 9): %v", err)
	}
}

func assertMissingWindowWritesNothing(t *testing.T, ctx context.Context, db *sql.DB, store *timescale.Store, bucket time.Time) {
	t.Helper()
	err := store.InsertPriceSourceContributions(ctx, []timescale.PriceSourceContribution{
		{AssetID: "crypto:SOL", QuoteID: "fiat:USD", Window: time.Hour, Bucket: bucket, Source: "kraken", Weight: 1, TradeCount: 1},
		{AssetID: "crypto:SOL", QuoteID: "fiat:USD", Bucket: bucket, Source: "binance", Weight: 1, TradeCount: 1},
	})
	if !errors.Is(err, timescale.ErrContributionWindowRequired) {
		t.Errorf("windowless row: err = %v, want ErrContributionWindowRequired", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM price_source_contributions WHERE asset_id = 'crypto:SOL'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("a batch holding a windowless row wrote %d rows, want 0", n)
	}
}

func assertRetentionPolicyDisabled(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var scheduled bool
	var dropAfter string
	if err := db.QueryRowContext(ctx, `
		SELECT scheduled, config->>'drop_after' FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_retention'
		   AND hypertable_name = 'price_source_contributions'`).Scan(&scheduled, &dropAfter); err != nil {
		t.Fatalf("read retention job: %v", err)
	}
	if scheduled {
		t.Error("0169's retention policy is scheduled; it must ship disabled")
	}
	if dropAfter != "90 days" {
		t.Errorf("retention drop_after = %q, want 90 days", dropAfter)
	}
}

func assertDownDropsWindowAndPolicy(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var cols, jobs, rows int
	if err := db.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns
		         WHERE table_name = 'price_source_contributions' AND column_name = 'window_seconds'),
		       (SELECT count(*) FROM timescaledb_information.jobs
		         WHERE proc_name = 'policy_retention' AND hypertable_name = 'price_source_contributions'),
		       (SELECT count(*) FROM price_source_contributions)`).Scan(&cols, &jobs, &rows); err != nil {
		t.Fatalf("read post-down state: %v", err)
	}
	if cols != 0 || jobs != 0 {
		t.Errorf("after 0169 down: window_seconds columns=%d retention jobs=%d, want 0 and 0", cols, jobs)
	}
	if rows != 5 {
		t.Errorf("after 0169 down: %d rows, want 5 (legacy + three windows + old-binary row)", rows)
	}
}
