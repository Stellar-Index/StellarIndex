//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/test/harness"
)

// TestOpenBackfillStore_StampsPositiveGeneration is the MR-1 regression (2)
// proven-red test (audit-2026-08-14). fx-history-backfill is an INV-3
// corrective entry point that writes historical fx_quotes rows over the key
// the live forex worker owns. Unlike every other corrective tool
// (backfill.go, backfill_external.go, supply.go, ch_rebuild.go) it never
// called store.SetDeriveGeneration, so it wrote at generation 0 and its
// operator corrections were silently reverted by the next daily gen-0 worker
// refresh (last-writer-wins).
//
// openBackfillStore is the fix: it stamps time.Now().Unix() (a POSITIVE
// generation) on the store before any write, so the fx_quotes generation
// guard (migration 0141) makes corrections durable. This test opens a real
// store through openBackfillStore — the exact seam main() uses — and asserts
// the store is in a POSITIVE-generation corrective mode.
//
// Reverting openBackfillStore to a plain timescale.Open (dropping the
// SetDeriveGeneration call) turns this red: DeriveGeneration() == 0.
func TestOpenBackfillStore_StampsPositiveGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// The one shared Timescale bootstrap (test/harness): openBackfillStore
	// only pings, so no migrations are applied on top of it.
	dsn := harness.StartTimescale(t, ctx)

	before := time.Now().Unix()
	store, err := openBackfillStore(ctx, dsn)
	if err != nil {
		t.Fatalf("openBackfillStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	gen := store.DeriveGeneration()
	if gen <= 0 {
		t.Fatalf("openBackfillStore stamped derive_generation=%d, want a POSITIVE value — "+
			"a gen-0 operator correction is silently reverted by the next live worker "+
			"refresh (MR-1 regression 2)", gen)
	}
	if gen < before {
		t.Errorf("derive_generation=%d predates the call (%d); expected ~time.Now().Unix()", gen, before)
	}
}
