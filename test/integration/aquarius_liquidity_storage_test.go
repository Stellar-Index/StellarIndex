//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"strings"
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

// TestLatestAquariusReserves exercises the READ side of the Aquarius
// TVL/depth signal (LatestAquariusReserves): empty-safe (nil, nil) on an
// empty table, latest-snapshot-per-pool selection (an older snapshot is
// shadowed by a newer one), per-token fan-out preserved, i128 NUMERIC
// round-trip, a drained (zero) leg surviving, and positional token-address
// resolution from aquarius_liquidity.
func TestLatestAquariusReserves(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Empty-safe: nothing captured yet.
	if pools, err := store.LatestAquariusReserves(ctx, 90); err != nil {
		t.Fatalf("LatestAquariusReserves (empty): %v", err)
	} else if pools != nil {
		t.Fatalf("LatestAquariusReserves (empty) = %+v, want nil", pools)
	}

	const (
		pool   = "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE"
		tokenA = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"
		tokenB = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY"
	)
	base := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	huge, _ := new(big.Int).SetString("98765432109876543210987654321", 10)

	// Older snapshot — must be shadowed by the newer one below.
	if err := store.InsertAquariusReserves(ctx, timescale.AquariusReservesEvent{
		ContractID:      pool,
		Ledger:          53_000_000,
		LedgerCloseTime: base,
		TxHash:          "1111111111111111111111111111111111111111111111111111111111111111",
		OpIndex:         0,
		EventIndex:      1,
		Reserves: []canonical.Amount{
			canonical.NewAmount(big.NewInt(1)),
			canonical.NewAmount(big.NewInt(2)),
		},
	}); err != nil {
		t.Fatalf("InsertAquariusReserves (older): %v", err)
	}

	// Newer snapshot — the one the reader must return. A huge i128 leg and a
	// drained (zero) leg.
	if err := store.InsertAquariusReserves(ctx, timescale.AquariusReservesEvent{
		ContractID:      pool,
		Ledger:          53_500_000,
		LedgerCloseTime: base.Add(time.Hour),
		TxHash:          "2222222222222222222222222222222222222222222222222222222222222222",
		OpIndex:         0,
		EventIndex:      2,
		Reserves: []canonical.Amount{
			canonical.NewAmount(huge),
			canonical.NewAmount(big.NewInt(0)),
		},
	}); err != nil {
		t.Fatalf("InsertAquariusReserves (newer): %v", err)
	}

	// A deposit so token_index → address resolves positionally.
	if err := store.InsertAquariusLiquidity(ctx, timescale.AquariusLiquidityEvent{
		ContractID:      pool,
		Ledger:          53_100_000,
		LedgerCloseTime: base.Add(30 * time.Minute),
		TxHash:          "3333333333333333333333333333333333333333333333333333333333333333",
		OpIndex:         0,
		EventIndex:      0,
		Action:          timescale.AquariusLiquidityDeposit,
		Tokens:          []string{tokenA, tokenB},
		Amounts: []canonical.Amount{
			canonical.NewAmount(big.NewInt(10)),
			canonical.NewAmount(big.NewInt(20)),
		},
		Shares: canonical.NewAmount(big.NewInt(5)),
	}); err != nil {
		t.Fatalf("InsertAquariusLiquidity: %v", err)
	}

	pools, err := store.LatestAquariusReserves(ctx, 90)
	if err != nil {
		t.Fatalf("LatestAquariusReserves: %v", err)
	}
	if len(pools) != 1 {
		t.Fatalf("pools = %d, want 1", len(pools))
	}
	p := pools[0]
	if p.ContractID != pool {
		t.Errorf("pool = %q, want %q", p.ContractID, pool)
	}
	if p.Ledger != 53_500_000 {
		t.Errorf("ledger = %d, want 53500000 (latest snapshot, not the older one)", p.Ledger)
	}
	if len(p.Legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(p.Legs))
	}
	// i128 preserved on the big leg; zero leg survived.
	if p.Legs[0].Reserve.BigInt().Cmp(huge) != 0 {
		t.Errorf("leg[0].Reserve = %s, want %s — i128/NUMERIC lost precision", p.Legs[0].Reserve, huge)
	}
	if p.Legs[1].Reserve.Sign() != 0 {
		t.Errorf("leg[1].Reserve = %s, want 0 (drained leg)", p.Legs[1].Reserve)
	}
	// Token addresses resolved positionally from the deposit.
	if p.Legs[0].Token != tokenA {
		t.Errorf("leg[0].Token = %q, want %q", p.Legs[0].Token, tokenA)
	}
	if p.Legs[1].Token != tokenB {
		t.Errorf("leg[1].Token = %q, want %q", p.Legs[1].Token, tokenB)
	}
}

// TestBespokeAquariusReservesSurfaced proves the captured-but-not-surfaced
// gap is closed: the Aquarius bespoke DEX block gains the reserve-derived
// TVL/depth KPI + pool-depth table once reserves exist, and stays empty-safe
// (nil block, not an error) when neither trades nor reserves are captured —
// so the panel renders cleanly on an r1 that has not yet captured a reserve
// snapshot. No frontend change is needed; the block renders generically.
func TestBespokeAquariusReservesSurfaced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Empty-safe: no trades, no reserves → nil block (not an error).
	blk, err := store.BuildProtocolBespoke(ctx, "aquarius", "amm", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke (empty): %v", err)
	}
	if blk != nil {
		t.Fatalf("BuildProtocolBespoke (empty) = %+v, want nil", blk)
	}

	// Capture a reserve snapshot (no trades needed — depth is independent of
	// volume).
	const pool = "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE"
	if err := store.InsertAquariusReserves(ctx, timescale.AquariusReservesEvent{
		ContractID:      pool,
		Ledger:          53_500_000,
		LedgerCloseTime: time.Now().UTC().Add(-24 * time.Hour),
		TxHash:          "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		OpIndex:         0,
		EventIndex:      0,
		Reserves: []canonical.Amount{
			canonical.NewAmount(big.NewInt(123_456_789)),
			canonical.NewAmount(big.NewInt(987_654_321)),
		},
	}); err != nil {
		t.Fatalf("InsertAquariusReserves: %v", err)
	}

	blk, err = store.BuildProtocolBespoke(ctx, "aquarius", "amm", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke = nil, want a block carrying the reserve TVL/depth KPI")
	}

	var hasDepthKPI bool
	for _, k := range blk.KPIs {
		if strings.HasPrefix(k.Label, "Pools with live reserves") {
			hasDepthKPI = true
			if k.Value != "1" {
				t.Errorf("depth KPI value = %q, want \"1\"", k.Value)
			}
		}
	}
	if !hasDepthKPI {
		t.Errorf("bespoke block missing the 'Pools with live reserves' TVL/depth KPI; KPIs=%+v", blk.KPIs)
	}

	var hasDepthTable bool
	for _, tb := range blk.Tables {
		if tb.Title == "Pool liquidity depth (latest reserves)" {
			hasDepthTable = true
			if len(tb.Rows) != 2 {
				t.Errorf("depth table rows = %d, want 2 (two token legs)", len(tb.Rows))
			}
		}
	}
	if !hasDepthTable {
		t.Errorf("bespoke block missing the pool-depth table; Tables=%+v", blk.Tables)
	}
}
