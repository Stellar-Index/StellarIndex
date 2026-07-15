//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestNonstandardDecimalsAssets_UpsertAndLoad exercises the round trip
// backing the dex-nonstandard-decimals read-time serving guard (migration
// 0093): the aggregator's decimals-guard sweep upserts a confirmed
// offender via UpsertNonstandardDecimalsAsset; the API's
// NonstandardDecimalsCache loads the full set via
// LoadNonstandardDecimalsAssets on its refresh cadence.
//
// Proves: empty-safe (nothing inserted yet → empty slice, not an error),
// a fresh insert round-trips faithfully, and a re-confirmation of the same
// asset (ON CONFLICT DO UPDATE) refreshes decimals/source/confirmed_at
// rather than producing a duplicate row — the guard's dedup latch means
// this should be rare in practice, but the upsert must still be safe if a
// process restart re-confirms the same standing offender.
func TestNonstandardDecimalsAssets_UpsertAndLoad(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Empty-safe.
	if rows, err := store.LoadNonstandardDecimalsAssets(ctx); err != nil {
		t.Fatalf("LoadNonstandardDecimalsAssets (empty): %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("LoadNonstandardDecimalsAssets (empty) = %d rows, want 0", len(rows))
	}

	const asset = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"

	if err := store.UpsertNonstandardDecimalsAsset(ctx, asset, 9, "aquarius"); err != nil {
		t.Fatalf("UpsertNonstandardDecimalsAsset: %v", err)
	}

	rows, err := store.LoadNonstandardDecimalsAssets(ctx)
	if err != nil {
		t.Fatalf("LoadNonstandardDecimalsAssets: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("LoadNonstandardDecimalsAssets = %d rows, want 1", len(rows))
	}
	if rows[0].Asset != asset || rows[0].Decimals != 9 || rows[0].Source != "aquarius" {
		t.Fatalf("row = %+v, want {Asset:%s Decimals:9 Source:aquarius}", rows[0], asset)
	}
	if rows[0].ConfirmedAt.IsZero() {
		t.Fatal("ConfirmedAt is zero, want a real timestamp (DEFAULT now())")
	}
	firstConfirmedAt := rows[0].ConfirmedAt

	// Re-confirmation (e.g. a process restart re-observing the same
	// standing offender) must upsert in place, not duplicate.
	time.Sleep(10 * time.Millisecond) // ensure a distinguishable now() on refresh
	if err := store.UpsertNonstandardDecimalsAsset(ctx, asset, 9, "phoenix"); err != nil {
		t.Fatalf("UpsertNonstandardDecimalsAsset (re-confirm): %v", err)
	}
	rows, err = store.LoadNonstandardDecimalsAssets(ctx)
	if err != nil {
		t.Fatalf("LoadNonstandardDecimalsAssets (after re-confirm): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("LoadNonstandardDecimalsAssets (after re-confirm) = %d rows, want 1 (upsert, not insert)", len(rows))
	}
	if rows[0].Source != "phoenix" {
		t.Fatalf("Source = %s, want phoenix (re-confirm should refresh source)", rows[0].Source)
	}
	if !rows[0].ConfirmedAt.After(firstConfirmedAt) {
		t.Fatalf("ConfirmedAt did not advance on re-confirm: first=%v second=%v", firstConfirmedAt, rows[0].ConfirmedAt)
	}
}
