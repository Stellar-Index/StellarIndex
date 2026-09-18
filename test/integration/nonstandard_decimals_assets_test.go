//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/decimalsguard"
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

	// Delete (the lockstep reconcile's repair when the lake confirms 7 dp)
	// removes the row; a second delete of the now-absent row is a no-op,
	// not an error.
	if err := store.DeleteNonstandardDecimalsAsset(ctx, asset); err != nil {
		t.Fatalf("DeleteNonstandardDecimalsAsset: %v", err)
	}
	rows, err = store.LoadNonstandardDecimalsAssets(ctx)
	if err != nil {
		t.Fatalf("LoadNonstandardDecimalsAssets (after delete): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("LoadNonstandardDecimalsAssets (after delete) = %d rows, want 0", len(rows))
	}
	if err := store.DeleteNonstandardDecimalsAsset(ctx, asset); err != nil {
		t.Fatalf("DeleteNonstandardDecimalsAsset (absent row): %v", err)
	}
}

// lockstepTradeReader is a decimalsguard.TradeReader that enumerates
// nothing — Reconcile does not consult trades, and this keeps the test
// about the projection table.
type lockstepTradeReader struct{}

func (lockstepTradeReader) RecentSorobanDEXTrades(_ context.Context, _ time.Time) ([]timescale.SorobanDEXTradeRef, error) {
	return nil, nil
}

// lockstepResolver stands in for the lake: present ⇒ found.
type lockstepResolver map[string]uint32

func (r lockstepResolver) TokenDecimals(_ context.Context, contractID string) (uint32, bool, error) {
	d, ok := r[contractID]
	return d, ok, nil
}

// TestNonstandardDecimalsAssets_LockstepReconcileThroughStore runs the
// aggregator's lockstep reconcile (decimalsguard.Guard.Reconcile) against
// the REAL store on a real Postgres: the production wiring passes
// *timescale.Store as the guard's Writer, and the compile-time assertion
// in decimalsguard says it satisfies the reconcile seam — this proves the
// three statements behind that seam (load, upsert-repair, delete-repair)
// execute and converge the table on the lake.
func TestNonstandardDecimalsAssets_LockstepReconcileThroughStore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		drifted      = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J" // persisted 6, lake 8
		staleStd     = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO" // persisted 9, lake 7
		agreed       = "CBI7UCH5KGSVQRO5H4SUCZUTZABCITZLRHQQZTWL2TK4RZ72TAR6IHRV" // persisted 18, lake 18
		unresolvable = "CDPV3H7C3MR2R4Y4GAEJN4AXXY4LBITRRVE74VSMVCSBWISIU3Q4QTMW" // persisted 6, lake not derivable
	)
	seed := map[string]uint32{drifted: 6, staleStd: 9, agreed: 18, unresolvable: 6}
	for asset, d := range seed {
		if err := store.UpsertNonstandardDecimalsAsset(ctx, asset, d, "aquarius"); err != nil {
			t.Fatalf("seed %s: %v", asset, err)
		}
	}

	lake := lockstepResolver{drifted: 8, staleStd: 7, agreed: 18}
	guard := decimalsguard.New(lockstepTradeReader{}, lake, decimalsguard.Options{Writer: store})
	if err := guard.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rows, err := store.LoadNonstandardDecimalsAssets(ctx)
	if err != nil {
		t.Fatalf("LoadNonstandardDecimalsAssets (after reconcile): %v", err)
	}
	got := make(map[string]int, len(rows))
	for _, r := range rows {
		got[r.Asset] = r.Decimals
	}
	want := map[string]int{drifted: 8, agreed: 18, unresolvable: 6}
	if len(got) != len(want) {
		t.Fatalf("rows after reconcile = %v, want %v", got, want)
	}
	for asset, d := range want {
		if got[asset] != d {
			t.Errorf("row %s = %d, want %d", asset, got[asset], d)
		}
	}
	if _, still := got[staleStd]; still {
		t.Errorf("row %s remains although the lake confirms 7 dp", staleStd)
	}
	// The invariant the reconcile exists for, checked against the lake:
	// every remaining row the lake can read equals the lake.
	for asset, d := range got {
		if l, ok := lake[asset]; ok && int(l) != d {
			t.Errorf("row %s persisted %d but the lake says %d", asset, d, l)
		}
	}
}
