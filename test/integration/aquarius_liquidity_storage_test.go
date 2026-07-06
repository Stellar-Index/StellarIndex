//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// TestAquariusReservesRoundTrip exercises InsertAquariusReserves
// through real TimescaleDB — the migration-0089 aquarius_reserves
// schema. Validates the per-token fan-out (N rows from one event, keyed
// by token_index), NUMERIC i128 preservation, the reserve >= 0 CHECK
// (a zero reserve is legal), and ON CONFLICT idempotency.
func TestAquariusReservesRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const pool = "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE"
	t0 := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)

	// A 3-token reserve vector: one big i128, one normal, one zero
	// (freshly drained leg — must not be rejected).
	ev := timescale.AquariusReservesEvent{
		ContractID:      pool,
		Ledger:          57_725_480,
		LedgerCloseTime: t0,
		TxHash:          "76cc361f2530929b738ed7f4e61c8ee9764281f7a3ef74904215fb4c0ce512e2",
		OpIndex:         0,
		EventIndex:      5,
		Reserves: []canonical.Amount{
			canonical.NewAmount(huge),
			canonical.NewAmount(big.NewInt(11_380_638_543_764)),
			canonical.NewAmount(big.NewInt(0)),
		},
	}
	if err := store.InsertAquariusReserves(ctx, ev); err != nil {
		t.Fatalf("InsertAquariusReserves: %v", err)
	}
	// Idempotent re-insert.
	if err := store.InsertAquariusReserves(ctx, ev); err != nil {
		t.Fatalf("InsertAquariusReserves (dup): %v", err)
	}

	var n int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM aquarius_reserves WHERE contract_id = $1`, pool).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("aquarius_reserves rows = %d, want 3 (per-token fan-out, idempotent)", n)
	}

	// i128 round-trips at token_index 0.
	var got canonical.Amount
	if err := store.DB().QueryRowContext(ctx,
		`SELECT reserve::text FROM aquarius_reserves WHERE contract_id = $1 AND token_index = 0`, pool).
		Scan(&got); err != nil {
		t.Fatalf("read reserve[0]: %v", err)
	}
	if got.BigInt().Cmp(huge) != 0 {
		t.Errorf("reserve[0] = %s, want %s — i128/NUMERIC lost precision", got, huge)
	}
}

// TestAquariusLiquidityRoundTrip exercises InsertAquariusLiquidity —
// the migration-0089 aquarius_liquidity schema. Validates the per-token
// fan-out, the deposit/withdraw action CHECK, that `shares` lands on
// token_index = 0 only (NULL elsewhere, so SUM(shares) is honest), and
// i128 preservation.
func TestAquariusLiquidityRoundTrip(t *testing.T) {
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
		pool   = "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE"
		tokenA = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"
		tokenB = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY"
	)
	t0 := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

	// deposit — two tokens, shares 30000000000.
	deposit := timescale.AquariusLiquidityEvent{
		ContractID:      pool,
		Ledger:          53_158_643,
		LedgerCloseTime: t0,
		TxHash:          "de0ffb02a16e43d721be2849f82d26bfec1bd533f2c25b7a453dd6e07b60ba3d",
		OpIndex:         0,
		EventIndex:      3,
		Action:          timescale.AquariusLiquidityDeposit,
		Tokens:          []string{tokenA, tokenB},
		Amounts: []canonical.Amount{
			canonical.NewAmount(big.NewInt(300_000_000_000)),
			canonical.NewAmount(big.NewInt(3_000_000_000_000)),
		},
		Shares: canonical.NewAmount(big.NewInt(30_000_000_000)),
	}
	if err := store.InsertAquariusLiquidity(ctx, deposit); err != nil {
		t.Fatalf("InsertAquariusLiquidity (deposit): %v", err)
	}
	if err := store.InsertAquariusLiquidity(ctx, deposit); err != nil { // idempotent
		t.Fatalf("InsertAquariusLiquidity (deposit dup): %v", err)
	}

	// withdraw — same pool, later ledger.
	if err := store.InsertAquariusLiquidity(ctx, timescale.AquariusLiquidityEvent{
		ContractID:      pool,
		Ledger:          53_317_087,
		LedgerCloseTime: t0.Add(time.Hour),
		TxHash:          "d9b47f6360660d2c36d8cafd21516a827b5e8794ef3c532663125f7d7fabb87c",
		OpIndex:         0,
		EventIndex:      4,
		Action:          timescale.AquariusLiquidityWithdraw,
		Tokens:          []string{tokenA, tokenB},
		Amounts: []canonical.Amount{
			canonical.NewAmount(big.NewInt(1_957_242_050)),
			canonical.NewAmount(big.NewInt(19_087_470_280)),
		},
		Shares: canonical.NewAmount(big.NewInt(200_870_534)),
	}); err != nil {
		t.Fatalf("InsertAquariusLiquidity (withdraw): %v", err)
	}

	// 4 rows total (2 per event), idempotent.
	var n int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM aquarius_liquidity WHERE contract_id = $1`, pool).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 4 {
		t.Fatalf("aquarius_liquidity rows = %d, want 4", n)
	}

	// shares present ONLY on token_index = 0.
	var sharesNonZeroIdx int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM aquarius_liquidity WHERE contract_id = $1 AND token_index <> 0 AND shares IS NOT NULL`,
		pool).Scan(&sharesNonZeroIdx); err != nil {
		t.Fatalf("shares NULL check: %v", err)
	}
	if sharesNonZeroIdx != 0 {
		t.Errorf("found %d rows with shares set on token_index<>0; want 0", sharesNonZeroIdx)
	}

	// SUM(shares) is honest (not N-counted): 30000000000 + 200870534.
	var sumShares canonical.Amount
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COALESCE(SUM(shares), 0)::text FROM aquarius_liquidity WHERE contract_id = $1`, pool).
		Scan(&sumShares); err != nil {
		t.Fatalf("sum shares: %v", err)
	}
	wantSum := big.NewInt(30_000_000_000 + 200_870_534)
	if sumShares.BigInt().Cmp(wantSum) != 0 {
		t.Errorf("SUM(shares) = %s, want %s", sumShares, wantSum)
	}
}
