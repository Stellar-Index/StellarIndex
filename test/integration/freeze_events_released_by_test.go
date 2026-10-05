//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestFreezeEvents_ReleasedBy executes migration 0208 and every write that
// touches the column: MarkRecovered on a row in a COMPRESSED chunk, on a live
// row, and the previous binary's UPDATE, which must keep working and leave it
// NULL (migrations/README.md rule 9).
func TestFreezeEvents_ReleasedBy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sink := timescale.NewFreezeEventSink(store)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	quote, _ := c.NewFiatAsset("USD")
	releasedBy := func(asset c.Asset) (sql.NullString, bool) {
		t.Helper()
		var by sql.NullString
		var closed bool
		if err := db.QueryRowContext(ctx, `
			SELECT released_by, recovered_at IS NOT NULL FROM freeze_events
			 WHERE asset_id = $1 AND quote_id = $2`, asset.String(), quote.String()).Scan(&by, &closed); err != nil {
			t.Fatalf("read released_by for %s: %v", asset.String(), err)
		}
		return by, closed
	}

	// 1. An open row in an old chunk, compressed before MarkRecovered runs.
	// Ledger 0: the sink has no ledger provider and stamps recovered_at_ledger 0.
	old := c.NativeAsset()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO freeze_events (asset_id, quote_id, frozen_at, frozen_at_ledger, reason, frozen_value)
		VALUES ($1, $2, TIMESTAMPTZ '2025-01-15 00:00:00Z', 0, 'outlier_storm', 0.5)`,
		old.String(), quote.String()); err != nil {
		t.Fatalf("seed old open row: %v", err)
	}
	var compressed int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT compress_chunk(ch) FROM show_chunks('freeze_events', older_than => now() - INTERVAL '90 days') ch
		) s`).Scan(&compressed); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if compressed == 0 {
		t.Fatal("no freeze_events chunk was compressed — the compressed-UPDATE claim would be vacuous")
	}
	if err := sink.MarkRecovered(ctx, old, quote, "operator:ash"); err != nil {
		t.Fatalf("MarkRecovered on a compressed chunk: %v", err)
	}
	if by, closed := releasedBy(old); !closed || by.String != "operator:ash" {
		t.Errorf("compressed row: closed=%v released_by=%v, want closed with operator:ash", closed, by)
	}

	// 2. A live row closed by the recovery worker's value.
	auto, _ := c.NewClassicAsset("USDT", "GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	decision := anomaly.Decision{Action: anomaly.ActionFreeze, Class: anomaly.ClassStablecoin}
	if err := sink.RecordFreeze(ctx, auto, quote, "1.000000000000", decision); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}
	if by, closed := releasedBy(auto); closed || by.Valid {
		t.Errorf("open row: closed=%v released_by=%v, want open with NULL", closed, by)
	}
	if err := sink.MarkRecovered(ctx, auto, quote, freeze.ReleasedBySystemRecovery); err != nil {
		t.Fatalf("MarkRecovered: %v", err)
	}
	if by, closed := releasedBy(auto); !closed || by.String != freeze.ReleasedBySystemRecovery {
		t.Errorf("auto row: closed=%v released_by=%v, want closed with %s", closed, by, freeze.ReleasedBySystemRecovery)
	}

	// 3. The pre-0208 binary's literal UPDATE against the 0208 schema.
	prev, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err := sink.RecordFreeze(ctx, prev, quote, "1.000000000000", decision); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE freeze_events
		   SET recovered_at        = $3,
		       recovered_at_ledger = $4
		 WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL`,
		prev.String(), quote.String(), time.Now().UTC(), int64(0))
	if err != nil {
		t.Fatalf("previous-binary MarkRecovered UPDATE: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("previous-binary UPDATE closed %d rows, want 1", n)
	}
	if by, closed := releasedBy(prev); !closed || by.Valid {
		t.Errorf("previous-binary row: closed=%v released_by=%v, want closed with NULL", closed, by)
	}
}
