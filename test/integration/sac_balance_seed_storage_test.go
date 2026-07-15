//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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

// TestSACBalanceSeedProvenanceRoundTrip exercises the sac_balance_seed_
// provenance audit table (migration 0102): a fresh contract has no row,
// the first upsert creates one, and a second upsert with different
// stats OVERWRITES it (one row per contract, not an append log) — the
// same "most recent pass" shape as sep41_supply_rollup's genesis-baseline
// columns (migration 0088).
func TestSACBalanceSeedProvenanceRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractID = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
	const assetKey = "PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"

	// Never seeded: absent, not an error.
	_, ok, err := store.SACBalanceSeedProvenanceFor(ctx, contractID)
	if err != nil {
		t.Fatalf("SACBalanceSeedProvenanceFor (never seeded): %v", err)
	}
	if ok {
		t.Fatal("ok=true for a never-seeded contract, want false")
	}

	// First pass: the default current-state source, 16 holders found.
	minL1, maxL1 := uint32(65_000_000), uint32(70_000_000)
	if err := store.UpsertSACBalanceSeedProvenance(ctx, timescale.SACBalanceSeedProvenance{
		ContractID:    contractID,
		AssetKey:      assetKey,
		Source:        timescale.SACBalanceSeedSourceCurrentState,
		HoldersSeeded: 16,
		MinLedgerSeen: &minL1,
		MaxLedgerSeen: &maxL1,
	}); err != nil {
		t.Fatalf("UpsertSACBalanceSeedProvenance (current_state): %v", err)
	}
	got, ok, err := store.SACBalanceSeedProvenanceFor(ctx, contractID)
	if err != nil {
		t.Fatalf("SACBalanceSeedProvenanceFor (after current_state seed): %v", err)
	}
	if !ok {
		t.Fatal("ok=false after an upsert, want true")
	}
	if got.Source != timescale.SACBalanceSeedSourceCurrentState || got.HoldersSeeded != 16 {
		t.Errorf("got=%+v, want source=current_state holders=16", got)
	}
	if got.MinLedgerSeen == nil || *got.MinLedgerSeen != 65_000_000 {
		t.Errorf("MinLedgerSeen = %v, want 65000000", got.MinLedgerSeen)
	}

	// Second pass: -full-history, reaching well below the ~62M floor —
	// OVERWRITES the row (one row per contract), evidencing the floor
	// was actually reached via a min_ledger_seen far below 62,000,000.
	minL2, maxL2 := uint32(41_500_000), uint32(70_000_000)
	if err := store.UpsertSACBalanceSeedProvenance(ctx, timescale.SACBalanceSeedProvenance{
		ContractID:    contractID,
		AssetKey:      assetKey,
		Source:        timescale.SACBalanceSeedSourceFullHistory,
		HoldersSeeded: 40,
		MinLedgerSeen: &minL2,
		MaxLedgerSeen: &maxL2,
	}); err != nil {
		t.Fatalf("UpsertSACBalanceSeedProvenance (full_history): %v", err)
	}
	got, ok, err = store.SACBalanceSeedProvenanceFor(ctx, contractID)
	if err != nil {
		t.Fatalf("SACBalanceSeedProvenanceFor (after full_history seed): %v", err)
	}
	if !ok {
		t.Fatal("ok=false after the second upsert, want true")
	}
	if got.Source != timescale.SACBalanceSeedSourceFullHistory {
		t.Errorf("Source = %q, want full_history (the second upsert should overwrite, not append)", got.Source)
	}
	if got.HoldersSeeded != 40 {
		t.Errorf("HoldersSeeded = %d, want 40", got.HoldersSeeded)
	}
	if got.MinLedgerSeen == nil || *got.MinLedgerSeen >= 62_000_000 {
		t.Errorf("MinLedgerSeen = %v, want < 62,000,000 (evidence the full-history pass reached below the current-state floor)", got.MinLedgerSeen)
	}

	// A wrapper with zero holders found this pass gets nil ledger bounds,
	// not a zero-valued (and misleading — ledger 0 is a real ledger)
	// min/max pair.
	const emptyContract = "CEMPTYWRAPPERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err := store.UpsertSACBalanceSeedProvenance(ctx, timescale.SACBalanceSeedProvenance{
		ContractID: emptyContract,
		AssetKey:   "NOPE:GISSUER",
		Source:     timescale.SACBalanceSeedSourceFullHistory,
	}); err != nil {
		t.Fatalf("UpsertSACBalanceSeedProvenance (zero holders): %v", err)
	}
	got, ok, err = store.SACBalanceSeedProvenanceFor(ctx, emptyContract)
	if err != nil {
		t.Fatalf("SACBalanceSeedProvenanceFor (zero holders): %v", err)
	}
	if !ok {
		t.Fatal("ok=false for the zero-holders row, want true (we DID seed, just found nothing)")
	}
	if got.HoldersSeeded != 0 {
		t.Errorf("HoldersSeeded = %d, want 0", got.HoldersSeeded)
	}
	if got.MinLedgerSeen != nil || got.MaxLedgerSeen != nil {
		t.Errorf("MinLedgerSeen=%v MaxLedgerSeen=%v, want both nil for a zero-holder pass", got.MinLedgerSeen, got.MaxLedgerSeen)
	}
}
