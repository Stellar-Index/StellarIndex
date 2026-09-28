//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAccountObservationSeedProvenanceRoundTrip executes migration 0189's
// table through the store (GH #1201): unstamped reads ok=false, a complete
// pass' upsert round-trips every column, and a second complete pass
// overwrites the singleton row rather than adding a second one.
func TestAccountObservationSeedProvenanceRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, ok, err := store.AccountObservationSeedProvenanceRow(ctx); err != nil || ok {
		t.Fatalf("never stamped: ok=%v err=%v, want ok=false", ok, err)
	}

	minL, maxL := uint32(30_000_000), uint32(63_400_000)
	first := timescale.AccountObservationSeedProvenance{
		AccountsWatched: 5, AccountsSeeded: 3, AccountsMissing: 1, AccountsRemoved: 1,
		MinLedgerSeen: &minL, MaxLedgerSeen: &maxL,
	}
	if err := store.UpsertAccountObservationSeedProvenance(ctx, first); err != nil {
		t.Fatalf("UpsertAccountObservationSeedProvenance: %v", err)
	}
	got, ok, err := store.AccountObservationSeedProvenanceRow(ctx)
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if got.AccountsWatched != 5 || got.AccountsSeeded != 3 || got.AccountsMissing != 1 || got.AccountsRemoved != 1 ||
		got.MinLedgerSeen == nil || *got.MinLedgerSeen != minL || got.MaxLedgerSeen == nil || *got.MaxLedgerSeen != maxL || got.SeededAt.IsZero() {
		t.Errorf("read back %+v, want %+v", got, first)
	}

	// A second complete pass — a re-seed weeks later that this time finds
	// every account already live — overwrites the one row; it must not
	// duplicate it (the primary key is the fixed scope, not a per-run id).
	second := timescale.AccountObservationSeedProvenance{AccountsWatched: 5, AccountsSeeded: 5}
	if err := store.UpsertAccountObservationSeedProvenance(ctx, second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, _, err = store.AccountObservationSeedProvenanceRow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountsWatched != 5 || got.AccountsSeeded != 5 || got.AccountsMissing != 0 || got.AccountsRemoved != 0 ||
		got.MinLedgerSeen != nil || got.MaxLedgerSeen != nil {
		t.Errorf("after overwrite %+v, want %+v with nil ledger bounds and no second row", got, second)
	}

	var rowCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT count(*) FROM account_observation_seed_provenance").Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("account_observation_seed_provenance has %d row(s), want exactly 1 (upsert, not insert)", rowCount)
	}
}
