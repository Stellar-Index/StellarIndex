//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSushiswapV3Pools_UpsertLoadRoundTrip executes migration 0200 and proves
// rows round-trip and a re-upsert replaces in place.
func TestSushiswapV3Pools_UpsertLoadRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	empty, err := store.LoadSushiswapV3Pools(ctx)
	if err != nil {
		t.Fatalf("Load (empty): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("Load (empty) = %#v, want non-nil empty slice", empty)
	}

	pool := contractStrkeyFromSeed(t, 0xD0)
	factory := contractStrkeyFromSeed(t, 0xD1)
	tok0 := contractStrkeyFromSeed(t, 0xD2)
	tok1 := contractStrkeyFromSeed(t, 0xD3)

	want := timescale.SushiswapV3Pool{
		PoolID: pool, FactoryID: factory, Token0: tok0, Token1: tok1,
		FeePips: 500, TickSpacing: 10, CreationLedger: 61_487_379,
	}
	if err := store.UpsertSushiswapV3Pool(ctx, want); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.UpsertSushiswapV3Pool(ctx, want); err != nil {
		t.Fatalf("re-Upsert: %v", err)
	}

	got, err := store.LoadSushiswapV3Pools(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Load = %#v, want [%#v]", got, want)
	}
}
