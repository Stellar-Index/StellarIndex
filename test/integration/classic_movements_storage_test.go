//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// mustAmount parses a decimal string into canonical.Amount, failing
// the test on error — local to this file since none of the other
// test/integration files needed a string-literal Amount constructor.
func mustAmount(t *testing.T, s string) canonical.Amount {
	t.Helper()
	a, err := canonical.FromString(s)
	if err != nil {
		t.Fatalf("canonical.FromString(%q): %v", s, err)
	}
	return a
}

// TestClassicMovementsAttributesRoundTrip exercises migration 0105's
// `attributes jsonb` column end to end through real TimescaleDB —
// ADR-0047 Phase 2 added Attributes to ClassicMovementRow /
// BatchInsertClassicMovements; a Go-layer marshal bug or a driver
// []byte->jsonb mismatch ships silently without this (unit tests
// only exercise marshalClassicMovementAttributes in isolation, never
// a real jsonb column).
func TestClassicMovementsAttributesRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	t0 := time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC)
	row := timescale.ClassicMovementRow{
		Kind:            timescale.ClassicMovementPathPayment,
		Ledger:          40_000_000,
		LedgerCloseTime: t0,
		TxHash:          "attrs-roundtrip-tx",
		OpIndex:         0,
		Asset:           "native",
		Amount:          mustAmount(t, "100"),
		FromAddress:     "GAFROM",
		ToAddress:       "GATOTO",
		Attributes: map[string]any{
			"send_asset":  "USDC-GISSUER",
			"send_amount": "50",
		},
	}
	landed, err := store.BatchInsertClassicMovements(ctx, []timescale.ClassicMovementRow{row})
	if err != nil {
		t.Fatalf("BatchInsertClassicMovements: %v", err)
	}
	if landed != 1 {
		t.Fatalf("landed = %d, want 1", landed)
	}

	var (
		gotSendAsset  string
		gotSendAmount string
	)
	err = store.DB().QueryRowContext(ctx, `
		SELECT attributes ->> 'send_asset', attributes ->> 'send_amount'
		FROM classic_movements
		WHERE tx_hash = $1
	`, "attrs-roundtrip-tx").Scan(&gotSendAsset, &gotSendAmount)
	if err != nil {
		t.Fatalf("query attributes: %v", err)
	}
	if gotSendAsset != "USDC-GISSUER" || gotSendAmount != "50" {
		t.Errorf("attributes = send_asset=%q send_amount=%q, want USDC-GISSUER / 50", gotSendAsset, gotSendAmount)
	}
}

// TestFindClaimableBalanceCreate_roundTrip exercises ADR-0047 Phase
// 3's second-pass correlation fallback against real Postgres: a
// 'claimable_balance_create' row written with a balance_id in its
// jsonb attributes must be findable by FindClaimableBalanceCreate,
// and a balance_id with no matching create must report found=false
// (never an error, never a guessed amount).
func TestFindClaimableBalanceCreate_roundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	t0 := time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC)
	const balanceID = "066245e9d37eac7223dcd81cd8513af66eaaf6a3d3e85c9fbef163e614ce009d"
	row := timescale.ClassicMovementRow{
		Kind:            timescale.ClassicMovementClaimableBalanceCreate,
		Ledger:          40_000_000,
		LedgerCloseTime: t0,
		TxHash:          "cb-create-tx",
		OpIndex:         0,
		Asset:           "GALA-GISSUER",
		Amount:          mustAmount(t, "387000"),
		FromAddress:     "GACREATOR",
		Attributes: map[string]any{
			"balance_id": balanceID,
			"claimants":  []string{"GACLAIMANT"},
		},
	}
	if _, err := store.BatchInsertClassicMovements(ctx, []timescale.ClassicMovementRow{row}); err != nil {
		t.Fatalf("BatchInsertClassicMovements: %v", err)
	}

	asset, amount, createdBy, found, err := store.FindClaimableBalanceCreate(ctx, balanceID)
	if err != nil {
		t.Fatalf("FindClaimableBalanceCreate: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if asset != "GALA-GISSUER" || amount.String() != "387000" || createdBy != "GACREATOR" {
		t.Errorf("got asset=%q amount=%q createdBy=%q, want GALA-GISSUER/387000/GACREATOR", asset, amount.String(), createdBy)
	}

	_, _, _, found, err = store.FindClaimableBalanceCreate(ctx, "0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatalf("FindClaimableBalanceCreate (missing): %v", err)
	}
	if found {
		t.Error("found = true for a balance_id with no create row, want false")
	}
}
