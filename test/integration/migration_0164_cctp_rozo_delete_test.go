//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// TestMigration0164_DeletesCompressedRowsAboveDMLDecompressCap applies 0164
// in r1's shape: cctp_events holds more compressed rows than
// timescaledb.max_tuples_decompressed_per_dml_transaction allows (148,660
// against 100,000 on r1). The unqualified DELETE must take the direct
// compressed-batch delete and succeed; the control shows the same DELETE
// fails on the cap when that path is off, so the test is not vacuous.
func TestMigration0164_DeletesCompressedRowsAboveDMLDecompressCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 163)

	const cctpRows = 120_001
	// 40 s apart spans ~56 days: nine 7-day chunks.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO cctp_events (contract_id, ledger, tx_hash, op_index, ts, event_type)
		SELECT 'CCTPTESTCONTRACT', g, lpad(to_hex(g), 64, '0'), 0,
		       TIMESTAMPTZ '2026-01-01 00:00:00Z' + g * INTERVAL '40 seconds', 'message_sent'
		  FROM generate_series(1, $1) g`, cctpRows); err != nil {
		t.Fatalf("seed cctp_events: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rozo_events (contract_id, ledger, tx_hash, op_index, ts, event_type, amount, destination)
		SELECT 'ROZOTESTCONTRACT', g, lpad(to_hex(g), 64, '0'), 0,
		       TIMESTAMPTZ '2026-01-01 00:00:00Z' + g * INTERVAL '10 days', 'payment', 1, 'GDEST'
		  FROM generate_series(1, 5) g`); err != nil {
		t.Fatalf("seed rozo_events: %v", err)
	}
	for _, table := range []string{"cctp_events", "rozo_events"} {
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM (SELECT compress_chunk(c) FROM show_chunks($1::regclass) c) s`,
			table).Scan(&n); err != nil {
			t.Fatalf("compress %s: %v", table, err)
		}
		if n < 2 {
			t.Fatalf("compressed %d %s chunks, want >= 2", n, table)
		}
	}

	var capSetting string
	if err := db.QueryRowContext(ctx,
		`SELECT current_setting('timescaledb.max_tuples_decompressed_per_dml_transaction')`).Scan(&capSetting); err != nil {
		t.Fatalf("read decompress cap: %v", err)
	}
	if capSetting != "100000" {
		t.Fatalf("max_tuples_decompressed_per_dml_transaction = %s, want the default 100000 (r1's value)", capSetting)
	}

	// Control: without the direct batch delete the same statement decompresses
	// and trips the cap.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin control tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL timescaledb.enable_compressed_direct_batch_delete = off`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("disable direct batch delete: %v", err)
	}
	_, ctlErr := tx.ExecContext(ctx, `DELETE FROM cctp_events`)
	_ = tx.Rollback()
	if ctlErr == nil || !strings.Contains(strings.ToLower(ctlErr.Error()), "decompress") {
		t.Fatalf("control DELETE with direct batch delete off: err = %v, want the decompression-limit error", ctlErr)
	}

	applyMigrationsUpTo(t, dsn, 164)
	requireSchemaVersion(t, ctx, db, 164)
	for _, table := range []string{"cctp_events", "rozo_events"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s has %d rows after 0164, want 0", table, n)
		}
	}
}
