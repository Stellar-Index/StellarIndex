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

// TestFreezeWindowLadders_Migration0163Down_KeepsTheLiveFreeze EXECUTES the
// 0163 down beside a compressed chunk and an open freeze that carries
// per-window ladders, and pins what migrations/README.md promises of it:
// the per-window ownership is lost, the pair-level summary — and so the
// freeze itself — is not.
//
// The summary is what survives because the writer keeps the 0119 columns as
// the fail-closed fold of the windows, so after the drop the row still says
// "escalated, held until the furthest hold" to every pair-level reader.
func TestFreezeWindowLadders_Migration0163Down_KeepsTheLiveFreeze(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	seedAndCompressFreezeChunk(t, ctx, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	sink := timescale.NewFreezeEventSink(store)
	asset, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	quote, _ := c.NewFiatAsset("USD")
	if err := sink.RecordFreeze(ctx, asset, quote, "0.874500000000", anomaly.Decision{
		Action: anomaly.ActionFreeze, Class: anomaly.ClassStablecoin, Reason: "phase2:3_signal_AND",
	}); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	escalated := freeze.State{
		FiredAt: now.Add(-2 * time.Hour), HoldUntil: now.Add(25 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions, Escalated: true,
	}
	fresh := freeze.State{FiredAt: now.Add(-time.Minute), HoldUntil: now.Add(9 * time.Minute)}
	if err := sink.SaveWindowLadder(ctx, asset, quote, time.Hour, escalated); err != nil {
		t.Fatalf("SaveWindowLadder(1h): %v", err)
	}
	if err := sink.SaveWindowLadder(ctx, asset, quote, 5*time.Minute, fresh); err != nil {
		t.Fatalf("SaveWindowLadder(5m): %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store close: %v", err)
	}

	applyMigrationsUpTo(t, dsn, 162) // executes the 0163 down
	assertFreezeChunkStillCompressed(t, ctx, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var cols int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'freeze_events' AND column_name = 'window_ladders'`).Scan(&cols); err != nil {
		t.Fatalf("column lookup: %v", err)
	}
	if cols != 0 {
		t.Errorf("window_ladders still present after the down (%d)", cols)
	}
	if n := countOpenFreezeRows(t, ctx, dsn, asset.String(), quote.String()); n != 1 {
		t.Fatalf("open freeze rows after the down = %d, want 1", n)
	}
	var (
		hold sql.NullTime
		esc  bool
		ext  int
	)
	if err := db.QueryRowContext(ctx, `
		SELECT hold_until, escalated, extensions_used
		  FROM freeze_events
		 WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL`,
		asset.String(), quote.String()).Scan(&hold, &esc, &ext); err != nil {
		t.Fatalf("read open freeze row: %v", err)
	}
	if !hold.Valid || !hold.Time.Equal(escalated.HoldUntil) || !esc || ext != freeze.DefaultMaxExtensions {
		t.Errorf("pair-level ladder after the down = (hold_until=%v valid=%v, escalated=%v, extensions_used=%d), "+
			"want (%v, true, %d): the down lost more than the new column",
			hold.Time.UTC(), hold.Valid, esc, ext, escalated.HoldUntil, freeze.DefaultMaxExtensions)
	}
}
