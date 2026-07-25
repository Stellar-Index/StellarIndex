//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCompletenessTargetFloorMonotonicity exercises the durable projection
// floor (migration 0116) against a real TimescaleDB.
//
// The floor exists because compute-completeness otherwise derives its
// reconcile floor from MIN(ledger) of the target it is checking, which makes
// the check blind to the one event it exists to catch: delete the oldest
// served rows and MIN(ledger) rises with the loss, so the range follows it
// up, the surviving rows reconcile perfectly, and the verdict reads
// complete. "Rows were dropped" and "we never projected below there" produce
// identical output.
//
// The single property that makes the remembered floor trustworthy is that it
// only ever FALLS. If a run starting higher could overwrite it, a partial or
// deliberately-narrowed run would ratchet the floor up to the post-loss MIN
// and silently restore the original bug. That is what this test pins —
// against the real database, because LEAST() semantics and the ON CONFLICT
// clause are SQL behaviour, not Go behaviour, and a mock would only prove I
// can write down what I already believe.
func TestCompletenessTargetFloorMonotonicity(t *testing.T) {
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
		src   = "soroswap"
		table = "trades"
		filt  = "source = 'soroswap'"
	)
	key := timescale.TargetFloorKey(src, table, filt)

	// 1. Absent target must be ABSENT, not zero. A caller that read a
	//    missing row as floor=0 would report "loss below 0" on the first
	//    run after the migration, for every target at once.
	floors, err := store.CompletenessTargetFloors(ctx)
	if err != nil {
		t.Fatalf("CompletenessTargetFloors (empty): %v", err)
	}
	if _, ok := floors[key]; ok {
		t.Fatalf("expected no floor recorded before the first upsert, got %+v", floors[key])
	}

	// 2. First write establishes the floor.
	if err := store.UpsertCompletenessTargetFloor(ctx, timescale.CompletenessTargetFloor{
		Source: src, Table: table, Filter: filt, VerifiedFrom: 61_500_000,
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if got := mustFloor(t, ctx, store, key); got != 61_500_000 {
		t.Errorf("after first upsert = %d, want 61500000", got)
	}

	// 3. A HIGHER value must NOT raise the floor. This is the regression
	//    that matters: a rising MIN(ledger) is exactly what loss looks
	//    like, so recording it would erase the evidence.
	if err := store.UpsertCompletenessTargetFloor(ctx, timescale.CompletenessTargetFloor{
		Source: src, Table: table, Filter: filt, VerifiedFrom: 71_000_000,
	}); err != nil {
		t.Fatalf("higher upsert: %v", err)
	}
	if got := mustFloor(t, ctx, store, key); got != 61_500_000 {
		t.Errorf("after a HIGHER upsert = %d, want the floor to stay at 61500000 — "+
			"a rising floor lets a partial run ratchet past real loss", got)
	}

	// 4. A LOWER value legitimately lowers it: new ground was verified.
	if err := store.UpsertCompletenessTargetFloor(ctx, timescale.CompletenessTargetFloor{
		Source: src, Table: table, Filter: filt, VerifiedFrom: 2,
	}); err != nil {
		t.Fatalf("lower upsert: %v", err)
	}
	if got := mustFloor(t, ctx, store, key); got != 2 {
		t.Errorf("after a LOWER upsert = %d, want 2", got)
	}

	// 5. Targets are isolated by filter, not just by table. `trades` holds
	//    several sources' rows, and their true floors genuinely differ —
	//    if the filter were dropped from the key, the earliest source
	//    would drag every other source's floor down with it and mask loss
	//    in all of them.
	const otherFilt = "source = 'sdex'"
	otherKey := timescale.TargetFloorKey(src, table, otherFilt)
	if err := store.UpsertCompletenessTargetFloor(ctx, timescale.CompletenessTargetFloor{
		Source: src, Table: table, Filter: otherFilt, VerifiedFrom: 55_000_000,
	}); err != nil {
		t.Fatalf("other-filter upsert: %v", err)
	}
	if got := mustFloor(t, ctx, store, otherKey); got != 55_000_000 {
		t.Errorf("other-filter floor = %d, want 55000000", got)
	}
	if got := mustFloor(t, ctx, store, key); got != 2 {
		t.Errorf("original floor = %d after writing a different filter, want 2 — "+
			"whereFilter must be part of the target identity", got)
	}

	// 6. Same table+filter under a DIFFERENT source is a different target.
	otherSrcKey := timescale.TargetFloorKey("sdex", table, filt)
	if err := store.UpsertCompletenessTargetFloor(ctx, timescale.CompletenessTargetFloor{
		Source: "sdex", Table: table, Filter: filt, VerifiedFrom: 40_000_000,
	}); err != nil {
		t.Fatalf("other-source upsert: %v", err)
	}
	if got := mustFloor(t, ctx, store, otherSrcKey); got != 40_000_000 {
		t.Errorf("other-source floor = %d, want 40000000", got)
	}
	if got := mustFloor(t, ctx, store, key); got != 2 {
		t.Errorf("original floor = %d after writing a different source, want 2", got)
	}
}

func mustFloor(t *testing.T, ctx context.Context, store *timescale.Store, key string) uint32 {
	t.Helper()
	floors, err := store.CompletenessTargetFloors(ctx)
	if err != nil {
		t.Fatalf("CompletenessTargetFloors: %v", err)
	}
	f, ok := floors[key]
	if !ok {
		t.Fatalf("no floor recorded for key %q", key)
	}
	return f.VerifiedFrom
}
