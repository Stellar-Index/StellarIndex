//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// TestSACBalanceSeedSupersedeAndNumeric exercises the two properties the
// `supply seed-sac-balances` bootstrap relies on, through real
// TimescaleDB (the seed reuses Store.InsertSACBalanceObservation +
// SumSACBalancesAtOrBefore / SACBalanceForContractAtOrBefore):
//
//  1. SUPERSEDE / at-or-before-ledger ordering — a seed written at an
//     OLD ledger must NOT clobber a newer live observation. The readers
//     pick the most-recent row per (contract_id, holder) by ledger DESC,
//     so the higher-ledger row wins regardless of insertion order. This
//     is what makes seeding-then-live-observing (and re-seeding) safe.
//
//  2. NUMERIC round-trip — a dormant contract-held SAC balance larger
//     than 2^63 must survive the *big.Int → NUMERIC → *big.Int trip
//     intact (ADR-0003; the whole point of seeding dormant C-held
//     balances is that they can be huge — ~5.9988e14 for a single
//     Phoenix contract, and nothing caps the aggregate).
func TestSACBalanceSeedSupersedeAndNumeric(t *testing.T) {
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
		sac    = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
		asset  = "PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
		holder = "CCPTA5MVKZG7T3YQZ2X3M4E5EXAMPLEHOLDERZZZZZZZZZZZZZZZZ7"
	)
	t0 := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	insertSACBig := func(ledger uint32, bal *big.Int, at time.Time) {
		t.Helper()
		if err := store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
			ContractID: sac, AssetKey: asset, Holder: holder,
			Ledger: ledger, ObservedAt: at, Balance: bal,
		}); err != nil {
			t.Fatalf("InsertSACBalanceObservation @%d: %v", ledger, err)
		}
	}

	// (2) NUMERIC round-trip: a dormant C-held balance > 2^63.
	dormant, _ := new(big.Int).SetString("599880000000000000000", 10) // ~6e20 ≫ 2^63
	// Seed the dormant balance at an OLD ledger (the entry's true
	// last-modified ledger, before the live observer's window).
	insertSACBig(62_400_000, dormant, t0)

	got, err := store.SACBalanceForContractAtOrBefore(ctx, holder, asset, 70_000_000)
	if err != nil {
		t.Fatalf("SACBalanceForContractAtOrBefore: %v", err)
	}
	if got.Cmp(dormant) != 0 {
		t.Fatalf("round-trip balance = %s, want %s (NUMERIC truncation?)", got, dormant)
	}
	sum, _ := store.SumSACBalancesAtOrBefore(ctx, asset, 70_000_000)
	if sum.Cmp(dormant) != 0 {
		t.Fatalf("sum after seed = %s, want %s", sum, dormant)
	}

	// (1) SUPERSEDE: a LATER live observation at a higher ledger wins.
	live := big.NewInt(1_000_000)
	insertSACBig(65_000_000, live, t0.Add(time.Hour))
	got, _ = store.SACBalanceForContractAtOrBefore(ctx, holder, asset, 70_000_000)
	if got.Cmp(live) != 0 {
		t.Errorf("after live obs = %s, want %s (higher-ledger live must supersede the seed)", got, live)
	}

	// A re-seed at the OLD ledger must NOT clobber the newer live row.
	insertSACBig(62_400_000, dormant, t0)
	got, _ = store.SACBalanceForContractAtOrBefore(ctx, holder, asset, 70_000_000)
	if got.Cmp(live) != 0 {
		t.Errorf("after re-seed = %s, want %s (old-ledger re-seed must not clobber newer live obs)", got, live)
	}

	// At-or-before the seed ledger only, the dormant seed is the answer.
	got, _ = store.SACBalanceForContractAtOrBefore(ctx, holder, asset, 62_400_500)
	if got.Cmp(dormant) != 0 {
		t.Errorf("at-or-before seed ledger = %s, want %s", got, dormant)
	}
}
