//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestSEP41TransfersAmountCheck executes 0174 (T090) over two populated
// compressed chunks, the shape on which a bare CHECK ALTER corrupts on
// timescaledb 2.26.4. After it a negative or missing transfer amount is
// refused with the named CHECK — including into a recompressed chunk —
// legitimate shapes still land, and the down restores the pre-0174 schema.
func TestSEP41TransfersAmountCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 173)
	if err := insertSEP41TransferAmount(ctx, db, 0, "transfer", "-1"); err != nil {
		t.Fatalf("pre-0174 negative transfer amount was refused (%v): the schema already had a CHECK and this test would be vacuous", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM sep41_transfers`); err != nil {
		t.Fatalf("clear pre-0174 row: %v", err)
	}
	if err := insertSEP41TransferAmount(ctx, db, 1, "transfer", "7"); err != nil {
		t.Fatalf("seed 2026-09 row: %v", err)
	}
	seedCompressedSEP41TransferChunk(t, ctx, db)

	applyMigrationsUpTo(t, dsn, 174)
	seedCompressedSEP41TransferChunk(t, ctx, db)
	requireCompressedSEP41TransferChunks(t, ctx, db, 2, "recompressed after 0174")

	refused := []struct {
		kind, amount string
	}{
		{"transfer", "-1"},
		{"approve", "-170141183460469231731687303715884105728"},
		{"transfer", ""},
		{"approve", ""},
		{"set_admin", "-5"},
	}
	for i, c := range refused {
		err := insertSEP41TransferAmount(ctx, db, 10+i, c.kind, c.amount)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "sep41_transfers_amount_check" {
			t.Errorf("%s amount %q after 0174: err = %v, want check_violation on sep41_transfers_amount_check", c.kind, c.amount, err)
		}
	}

	accepted := []struct {
		kind, amount string
	}{
		{"transfer", "0"},
		{"approve", "170141183460469231731687303715884105727"},
		{"set_admin", ""},
		{"set_authorized", ""},
	}
	for i, c := range accepted {
		if err := insertSEP41TransferAmount(ctx, db, 20+i, c.kind, c.amount); err != nil {
			t.Errorf("%s amount %q after 0174 refused: %v", c.kind, c.amount, err)
		}
	}

	seedCompressedSEP41TransferChunk(t, ctx, db)
	applyMigrationsUpTo(t, dsn, 173)
	if err := insertSEP41TransferAmount(ctx, db, 30, "transfer", "-1"); err != nil {
		t.Fatalf("after 0174 down, negative transfer amount still refused: %v", err)
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
// compresses both chunks, the shape production is in when 0174 applies and
// after its compression policy next runs.
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
	// Counted from compress_chunk itself: reading timescaledb_information.chunks
	// before 0174 applies masks the 2.26.4 CHECK corruption this test pins.
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
