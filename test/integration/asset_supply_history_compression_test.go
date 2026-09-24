//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestAssetSupplyHistoryCompressionRestored pins that the migration chain
// converges on a compressed asset_supply_history even when 0030 half-applied.
// 0030 commits its decompress + constraint swap inside an explicit
// BEGIN/COMMIT and re-enables compression in a SEPARATE implicit transaction
// after it, so a failure there leaves compression off with version 30 dirty;
// the only recovery, `force 30`, records it applied and nothing after it
// restored compression until 0168.
func TestAssetSupplyHistoryCompressionRestored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 30)
	if !assetSupplyHistoryCompressionEnabled(t, ctx, db) {
		t.Fatal("after a clean 0030, asset_supply_history compression is disabled; want enabled")
	}

	// The state 0030 commits before its trailing ALTER: constraint swapped,
	// compression disabled.
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE asset_supply_history SET (timescaledb.compress = false)`); err != nil {
		t.Fatalf("simulate 0030's committed half: %v", err)
	}

	applyMigrations(t, dsn)

	if !assetSupplyHistoryCompressionEnabled(t, ctx, db) {
		t.Fatal("asset_supply_history compression still disabled at migrations head after a half-applied 0030")
	}
	var segmentby, orderby string
	if err := db.QueryRowContext(ctx, `
        SELECT coalesce(segmentby, ''), coalesce(orderby, '')
          FROM timescaledb_information.hypertable_compression_settings
         WHERE hypertable = 'asset_supply_history'::regclass`).Scan(&segmentby, &orderby); err != nil {
		t.Fatalf("read compression settings: %v", err)
	}
	if segmentby != "asset_key" || orderby != "\"time\" DESC" {
		t.Errorf("compression settings = (segmentby %q, orderby %q), want (asset_key, \"time\" DESC) as 0005 set them",
			segmentby, orderby)
	}
	assertPolicyAttached(t, db, ctx, "asset_supply_history", "policy_compression")

	// Down is forward-only (no-op); the healthy-path up above must be too.
	applyMigrationsUpTo(t, dsn, 166)
	if !assetSupplyHistoryCompressionEnabled(t, ctx, db) {
		t.Error("0168 down disabled asset_supply_history compression; it must be a no-op")
	}
}

func assetSupplyHistoryCompressionEnabled(t *testing.T, ctx context.Context, db *sql.DB) bool {
	t.Helper()
	var enabled bool
	if err := db.QueryRowContext(ctx, `
        SELECT compression_enabled FROM timescaledb_information.hypertables
         WHERE hypertable_name = 'asset_supply_history'`).Scan(&enabled); err != nil {
		t.Fatalf("read asset_supply_history compression_enabled: %v", err)
	}
	return enabled
}
