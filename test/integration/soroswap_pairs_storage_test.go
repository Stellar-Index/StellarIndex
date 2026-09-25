//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSoroswapPairs_UpsertLoadRoundTrip exercises UpsertSoroswapPair and
// LoadSoroswapPairRegistry against real TimescaleDB (migration 0016):
// empty table loads as a non-nil empty slice, rows round-trip, and a
// re-upsert on the same pair_strkey replaces the token mapping in place.
func TestSoroswapPairs_UpsertLoadRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	empty, err := store.LoadSoroswapPairRegistry(ctx)
	if err != nil {
		t.Fatalf("Load (empty): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("Load (empty) = %#v, want non-nil empty slice", empty)
	}

	pairA := contractStrkeyFromSeed(t, 0xC0)
	pairB := contractStrkeyFromSeed(t, 0xC1)
	tok0 := contractStrkeyFromSeed(t, 0xC2)
	tok1 := contractStrkeyFromSeed(t, 0xC3)
	tok2 := contractStrkeyFromSeed(t, 0xC4)

	for _, p := range []timescale.SoroswapPair{
		{PairStrkey: pairA, Token0Strkey: tok0, Token1Strkey: tok1},
		{PairStrkey: pairB, Token0Strkey: tok1, Token1Strkey: tok2},
	} {
		if err := store.UpsertSoroswapPair(ctx, p.PairStrkey, p.Token0Strkey, p.Token1Strkey); err != nil {
			t.Fatalf("Upsert %s: %v", p.PairStrkey, err)
		}
	}
	// Re-upsert pairA with a different mapping: must update, not duplicate.
	if err := store.UpsertSoroswapPair(ctx, pairA, tok2, tok0); err != nil {
		t.Fatalf("re-Upsert %s: %v", pairA, err)
	}

	got, err := store.LoadSoroswapPairRegistry(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]timescale.SoroswapPair{
		pairA: {PairStrkey: pairA, Token0Strkey: tok2, Token1Strkey: tok0},
		pairB: {PairStrkey: pairB, Token0Strkey: tok1, Token1Strkey: tok2},
	}
	if len(got) != len(want) {
		t.Fatalf("Load returned %d rows, want %d: %#v", len(got), len(want), got)
	}
	for _, p := range got {
		if w, ok := want[p.PairStrkey]; !ok || p != w {
			t.Errorf("row %#v, want %#v", p, w)
		}
	}
}

// TestSoroswapPairs_InsertIfAbsentNeverOverwrites pins the seed path's
// write: a pair the indexer already registered from an on-chain new_pair
// event keeps its mapping when the stellar-rpc seed reports different
// tokens, and an unregistered pair is inserted.
func TestSoroswapPairs_InsertIfAbsentNeverOverwrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	pairA := contractStrkeyFromSeed(t, 0xD0)
	pairB := contractStrkeyFromSeed(t, 0xD1)
	tok0 := contractStrkeyFromSeed(t, 0xD2)
	tok1 := contractStrkeyFromSeed(t, 0xD3)

	if err := store.UpsertSoroswapPair(ctx, pairA, tok0, tok1); err != nil {
		t.Fatalf("Upsert %s: %v", pairA, err)
	}
	inserted, err := store.InsertSoroswapPairIfAbsent(ctx, pairA, tok1, tok0)
	if err != nil {
		t.Fatalf("InsertIfAbsent %s (registered): %v", pairA, err)
	}
	if inserted {
		t.Errorf("InsertIfAbsent %s reported an insert over a registered pair", pairA)
	}
	inserted, err = store.InsertSoroswapPairIfAbsent(ctx, pairB, tok1, tok0)
	if err != nil {
		t.Fatalf("InsertIfAbsent %s (new): %v", pairB, err)
	}
	if !inserted {
		t.Errorf("InsertIfAbsent %s reported no insert for an unregistered pair", pairB)
	}

	got, err := store.LoadSoroswapPairRegistry(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]timescale.SoroswapPair{
		pairA: {PairStrkey: pairA, Token0Strkey: tok0, Token1Strkey: tok1},
		pairB: {PairStrkey: pairB, Token0Strkey: tok1, Token1Strkey: tok0},
	}
	if len(got) != len(want) {
		t.Fatalf("Load returned %d rows, want %d: %#v", len(got), len(want), got)
	}
	for _, p := range got {
		if w, ok := want[p.PairStrkey]; !ok || p != w {
			t.Errorf("row %#v, want %#v", p, w)
		}
	}
}
