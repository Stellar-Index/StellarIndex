//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestMigration0197_TradesCompressionClearsTheRollWindow pins both halves of
// the fix: the asset-character roll locks only chunks inside its 14-day
// window, and trades compress only after that window, so the two never
// contend for the same chunk.
func TestMigration0197_TradesCompressionClearsTheRollWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c.InstallAliasRegistry(nil)

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 196)
	requireSchemaVersion(t, ctx, db, 196)
	quiesceCAGGRefreshPolicies(t, ctx, db)
	if got := tradesCompressAfter(t, ctx, db); got != "7 days" {
		t.Fatalf("before 0197 trades compress_after = %q, want 7 days", got)
	}

	now := time.Now().UTC()
	for i, days := range []int{40, 2} {
		insertTxIndexTrade(t, ctx, db, "sdex", 59_000_000+i, fmt.Sprintf("rw%d", i), now.Add(-time.Duration(days)*24*time.Hour))
	}
	var oldChunk string
	if err := db.QueryRowContext(ctx, `
		SELECT c::text FROM show_chunks('trades', older_than => now() - interval '30 days') c`).Scan(&oldChunk); err != nil {
		t.Fatalf("find the 40-day-old chunk: %v", err)
	}

	applyMigrationsUpTo(t, dsn, 197)
	requireSchemaVersion(t, ctx, db, 197)
	if got := tradesCompressAfter(t, ctx, db); got != "15 days" {
		t.Fatalf("after 0197 trades compress_after = %q, want 15 days", got)
	}

	// A lock on a chunk outside the window must not stall the roll; before
	// the literal window it needed ACCESS SHARE on every chunk.
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `LOCK TABLE `+oldChunk+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock %s: %v", oldChunk, err)
	}
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer func() { _ = store.Close() }()
	rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
	defer rcancel()
	if err := store.RefreshAssetVolumeCharacter(rctx); err != nil {
		t.Fatalf("roll with a chunk outside its window locked: %v", err)
	}
	_ = holder.Rollback()

	applyMigrationsUpTo(t, dsn, 196)
	requireSchemaVersion(t, ctx, db, 196)
	if got := tradesCompressAfter(t, ctx, db); got != "7 days" {
		t.Errorf("after 0197 down trades compress_after = %q, want 7 days", got)
	}
}

func tradesCompressAfter(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var v string
	if err := db.QueryRowContext(ctx, `
		SELECT config->>'compress_after' FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_compression' AND hypertable_name = 'trades'`).Scan(&v); err != nil {
		t.Fatalf("read trades compress_after: %v", err)
	}
	return v
}
