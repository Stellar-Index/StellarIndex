//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestMigration0174_NoOpLeavesCompressedChunksCompressed pins the v0.92.1
// neutralisation of 0174: over two populated compressed chunks it
// decompresses nothing and adds no constraint, leaves the version clean at
// 174, and its down succeeds both on that no-op state and on the v0.92.0
// state, where the old body had added sep41_transfers_amount_check.
func TestMigration0174_NoOpLeavesCompressedChunksCompressed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 173)
	if err := insertSEP41TransferAmount(ctx, db, 1, "transfer", "7"); err != nil {
		t.Fatalf("seed 2026-09 row: %v", err)
	}
	seedCompressedSEP41TransferChunk(t, ctx, db)

	applyMigrationsUpTo(t, dsn, 174)
	requireCompressedSEP41TransferChunks(t, ctx, db, 2, "after the 0174 no-op")
	requireSEP41AmountCheck(t, ctx, db, false, "after the 0174 no-op")
	requireSchemaVersion(t, ctx, db, 174)
	if err := insertSEP41TransferAmount(ctx, db, 2, "set_admin", "-5"); err != nil {
		t.Fatalf("after the 0174 no-op a row the old CHECK refused was refused anyway (%v): something still adds the constraint", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM sep41_transfers WHERE op_index = 2`); err != nil {
		t.Fatalf("clear the probe row: %v", err)
	}

	applyMigrationsUpTo(t, dsn, 173)
	requireSEP41AmountCheck(t, ctx, db, false, "after 0174 down from the no-op")

	// The v0.92.0 state: the old body ran, so the CHECK exists at 174.
	if _, err := db.ExecContext(ctx, `SELECT decompress_chunk(c, true) FROM show_chunks('sep41_transfers') c`); err != nil {
		t.Fatalf("decompress for the v0.92.0 0174 state: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		ALTER TABLE sep41_transfers ADD CONSTRAINT sep41_transfers_amount_check
		    CHECK ((amount IS NULL OR amount >= 0)
		           AND (event_kind NOT IN ('transfer', 'approve') OR amount IS NOT NULL))`); err != nil {
		t.Fatalf("recreate the v0.92.0 0174 state: %v", err)
	}
	applyMigrationsUpTo(t, dsn, 174)
	requireSEP41AmountCheck(t, ctx, db, true, "v0.92.0 state after the 0174 no-op")

	// Recompress before the down: the ADD CONSTRAINT above ran against
	// decompressed chunks, and left uncompressed. The v0.92.0-state down
	// (DROP CONSTRAINT on a CHECK that actually exists) is only exercised
	// against production's shape if it runs against compressed chunks too.
	seedCompressedSEP41TransferChunk(t, ctx, db)
	requireCompressedSEP41TransferChunks(t, ctx, db, 2, "recompressed before the v0.92.0-state down")

	applyMigrationsUpTo(t, dsn, 173)
	requireSEP41AmountCheck(t, ctx, db, false, "after 0174 down from the v0.92.0 state")
}

func requireSEP41AmountCheck(t *testing.T, ctx context.Context, db *sql.DB, want bool, when string) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_constraint
		 WHERE conname = 'sep41_transfers_amount_check'
		   AND conrelid = 'sep41_transfers'::regclass`).Scan(&n); err != nil {
		t.Fatalf("read pg_constraint: %v", err)
	}
	if (n == 1) != want {
		t.Fatalf("%s: sep41_transfers_amount_check present = %v, want %v", when, n == 1, want)
	}
}

func requireSchemaVersion(t *testing.T, ctx context.Context, db *sql.DB, want int) {
	t.Helper()
	var version int
	var dirty bool
	if err := db.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if version != want || dirty {
		t.Fatalf("schema_migrations = %d dirty=%v, want %d clean", version, dirty, want)
	}
}

// insertSEP41TransferAmount writes one row at 2026-09-01 with op_index op;
// amount "" is SQL NULL.
func insertSEP41TransferAmount(ctx context.Context, db *sql.DB, op int, kind, amount string) error {
	var amt any
	if amount != "" {
		amt = amount
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO sep41_transfers (ledger_close_time, ledger, tx_hash, op_index,
		                             contract_id, event_kind, amount)
		VALUES (TIMESTAMPTZ '2026-09-01 00:00:00Z', 60000000, '\x01'::bytea, $1,
		        'CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC', $2, $3::numeric)`,
		op, kind, amt)
	return err
}

// seedCompressedSEP41TransferChunk upserts one valid row in an old chunk and
// compresses both chunks, the shape production is in when 0174 applies.
func seedCompressedSEP41TransferChunk(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sep41_transfers (ledger_close_time, ledger, tx_hash, op_index,
		                             contract_id, event_kind, amount)
		VALUES (TIMESTAMPTZ '2025-01-15 00:00:00Z', 55000000, '\x02'::bytea, 0,
		        'CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC', 'transfer', 42)
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed historical sep41_transfers row: %v", err)
	}
	// Counted from compress_chunk itself, not timescaledb_information.chunks.
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT compress_chunk(c, if_not_compressed => true) FROM show_chunks('sep41_transfers') c
		) s`).Scan(&n); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if n != 2 {
		t.Fatalf("compressed %d sep41_transfers chunks, want 2", n)
	}
}

func requireCompressedSEP41TransferChunks(t *testing.T, ctx context.Context, db *sql.DB, want int, when string) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM timescaledb_information.chunks
		 WHERE hypertable_name = 'sep41_transfers' AND is_compressed`).Scan(&n); err != nil {
		t.Fatalf("read chunk compression state: %v", err)
	}
	if n != want {
		t.Fatalf("%s: %d compressed sep41_transfers chunks, want %d", when, n, want)
	}
}
