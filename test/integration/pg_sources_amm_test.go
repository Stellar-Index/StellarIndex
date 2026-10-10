//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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
	// Relative to now, not a fixed calendar date: LatestAquariusReserves
	// filters on `ledger_close_time > now() - windowDays`, so a hardcoded
	// date eventually ages out of the 90-day window and the test starts
	// failing on its own regardless of what changed in the code.
	base := time.Now().UTC().Add(-2 * time.Hour)
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

const (
	aquariusRwPool   = "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE"
	aquariusRwRouter = "CBQDHNBFBZYE4MKPWBSJOPIYLW4SFSXAXUTSXJN76GNKYVYPCKWC6QUK"
	aquariusRwUserA  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	aquariusRwUserB  = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"

	// Two distinct claim_reward reward-token contracts (AQUA and USDC SACs).
	aquariusRwAquaSAC = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"
	aquariusRwUsdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
)

// TestAquariusRewardsLifetimeByKind exercises the LIFETIME per-kind
// rewards-gauge reader (AquariusRewardsLifetimeByKind, the v0.12 gap
// closed by migration 0099): every one of the twelve kinds comes back in
// migration-0099 census order, kinds with no captured rows still appear
// with Events=0 (never a missing row). It carries no amount — see
// TestAquariusRewardsClaimWindow for the per-token amount readback.
func TestAquariusRewardsLifetimeByKind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Empty table: all twelve kinds still returned, all zero.
	byKind, err := store.AquariusRewardsLifetimeByKind(ctx)
	if err != nil {
		t.Fatalf("AquariusRewardsLifetimeByKind (empty): %v", err)
	}
	if len(byKind) != 12 {
		t.Fatalf("AquariusRewardsLifetimeByKind (empty) = %d kinds, want 12", len(byKind))
	}
	for _, k := range byKind {
		if k.Events != 0 {
			t.Errorf("kind %q on empty table = %+v, want zero", k.Kind, k)
		}
	}

	t0 := time.Now().UTC().Add(-48 * time.Hour)
	claimHuge, _ := new(big.Int).SetString("55555555555555555555", 10) // > 2^63

	rows := []timescale.AquariusRewardsEvent{
		{
			ContractID: aquariusRwPool, Ledger: 62_000_000, LedgerCloseTime: t0, TxHash: pad64("a", 0), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusRewardsClaimReward, UserAddress: aquariusRwUserA, Amount: amtPtr(canonical.NewAmount(claimHuge)),
		},
		{
			ContractID: aquariusRwPool, Ledger: 62_000_001, LedgerCloseTime: t0.Add(time.Minute), TxHash: pad64("a", 1), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusRewardsClaimReward, UserAddress: aquariusRwUserB, Amount: amtPtr(canonical.NewAmount(big.NewInt(2_000))),
		},
		{
			ContractID: aquariusRwPool, Ledger: 62_000_002, LedgerCloseTime: t0.Add(2 * time.Minute), TxHash: pad64("a", 2), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusRewardsPoolState,
		},
		{
			ContractID: aquariusRwRouter, Ledger: 62_000_003, LedgerCloseTime: t0.Add(3 * time.Minute), TxHash: pad64("a", 3), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusRewardsConfigRewards,
		},
	}
	for _, e := range rows {
		if err := store.InsertAquariusRewardsEvent(ctx, e); err != nil {
			t.Fatalf("InsertAquariusRewardsEvent %s: %v", e.Kind, err)
		}
		// Idempotent re-insert.
		if err := store.InsertAquariusRewardsEvent(ctx, e); err != nil {
			t.Fatalf("InsertAquariusRewardsEvent %s (dup): %v", e.Kind, err)
		}
	}

	byKind, err = store.AquariusRewardsLifetimeByKind(ctx)
	if err != nil {
		t.Fatalf("AquariusRewardsLifetimeByKind: %v", err)
	}
	if len(byKind) != 12 {
		t.Fatalf("AquariusRewardsLifetimeByKind = %d kinds, want 12", len(byKind))
	}
	// Census order: pool_state is first, claim_reward second.
	if byKind[0].Kind != timescale.AquariusRewardsPoolState || byKind[0].Events != 1 {
		t.Errorf("byKind[0] = %+v, want {pool_state 1}", byKind[0])
	}
	if byKind[1].Kind != timescale.AquariusRewardsClaimReward || byKind[1].Events != 2 {
		t.Errorf("byKind[1] = %+v, want {claim_reward 2}", byKind[1])
	}
	// config_rewards (router-side) is last in census order.
	last := byKind[len(byKind)-1]
	if last.Kind != timescale.AquariusRewardsConfigRewards || last.Events != 1 {
		t.Errorf("byKind[last] = %+v, want {config_rewards 1}", last)
	}
	// A kind nothing was inserted for stays at zero (never dropped from the set).
	if byKind[5].Kind != timescale.AquariusRewardsClaimFees || byKind[5].Events != 0 {
		t.Errorf("byKind[5] (claim_fees) = %+v, want zero events", byKind[5])
	}
}

// TestAquariusRewardsClaimWindow exercises the windowed claim_reward
// drill-down (AquariusRewardsClaimWindow): empty-safe on no claims, a
// claim outside the window is excluded (sargable ledger_close_time bound),
// distinct claimants dedupes a repeat claimant, volume is summed PER reward
// token (never across tokens), busiest token first, and i128 amounts
// survive the NUMERIC round trip.
func TestAquariusRewardsClaimWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if w, err := store.AquariusRewardsClaimWindow(ctx, 30); err != nil {
		t.Fatalf("AquariusRewardsClaimWindow (empty): %v", err)
	} else if w != nil {
		t.Fatalf("AquariusRewardsClaimWindow (empty) = %+v, want nil", w)
	}

	inWindow := time.Now().UTC().Add(-5 * 24 * time.Hour)
	outOfWindow := time.Now().UTC().Add(-90 * 24 * time.Hour)
	claimHuge, _ := new(big.Int).SetString("55555555555555555555", 10) // > 2^63

	claims := []timescale.AquariusRewardsEvent{
		// Two AQUA claims inside the 30d window, same claimant (dedupes to 1 distinct).
		aquariusClaim(63_000_000, inWindow, "c", 0, aquariusRwUserA, aquariusRwAquaSAC, big.NewInt(1_000)),
		aquariusClaim(63_000_001, inWindow.Add(time.Hour), "c", 1, aquariusRwUserA, aquariusRwAquaSAC, claimHuge),
		// A different claimant paid in a different reward token, also inside the window.
		aquariusClaim(63_000_002, inWindow.Add(2*time.Hour), "c", 2, aquariusRwUserB, aquariusRwUsdcSAC, big.NewInt(250)),
		// Outside the 30d window — must be excluded.
		aquariusClaim(60_000_000, outOfWindow, "c", 3, aquariusRwUserA, aquariusRwAquaSAC, big.NewInt(999_999)),
	}
	for _, e := range claims {
		if err := store.InsertAquariusRewardsEvent(ctx, e); err != nil {
			t.Fatalf("InsertAquariusRewardsEvent: %v", err)
		}
	}

	w, err := store.AquariusRewardsClaimWindow(ctx, 30)
	if err != nil {
		t.Fatalf("AquariusRewardsClaimWindow: %v", err)
	}
	if w == nil {
		t.Fatal("AquariusRewardsClaimWindow = nil, want a summary")
	}
	if w.Events != 3 {
		t.Errorf("Events = %d, want 3 (the out-of-window claim excluded)", w.Events)
	}
	if w.DistinctClaimants != 2 {
		t.Errorf("DistinctClaimants = %d, want 2", w.DistinctClaimants)
	}
	if len(w.ByToken) != 2 {
		t.Fatalf("ByToken = %+v, want one entry per reward token (2)", w.ByToken)
	}
	wantAqua := new(big.Int).Add(claimHuge, big.NewInt(1_000))
	if got := w.ByToken[0]; got.RewardToken != aquariusRwAquaSAC || got.Events != 2 || got.Amount.BigInt().Cmp(wantAqua) != 0 {
		t.Errorf("ByToken[0] = {%s %d %s}, want {%s 2 %s} — busiest token first, i128 exact", got.RewardToken, got.Events, got.Amount, aquariusRwAquaSAC, wantAqua)
	}
	if got := w.ByToken[1]; got.RewardToken != aquariusRwUsdcSAC || got.Events != 1 || got.Amount.BigInt().Cmp(big.NewInt(250)) != 0 {
		t.Errorf("ByToken[1] = {%s %d %s}, want {%s 1 250}", got.RewardToken, got.Events, got.Amount, aquariusRwUsdcSAC)
	}
}

// TestBespokeAquariusRewardVolumeNotSummedAcrossTokens pins that the
// Aquarius bespoke block never publishes a 30d reward volume that adds base
// units of different reward tokens: with claims paid in two tokens there is
// no scalar "Reward volume (30d)" KPI, and the per-token table carries each
// token's own sum.
func TestBespokeAquariusRewardVolumeNotSummedAcrossTokens(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	claimT := time.Now().UTC().Add(-3 * 24 * time.Hour)
	for _, e := range []timescale.AquariusRewardsEvent{
		aquariusClaim(63_600_000, claimT, "v", 0, aquariusRwUserA, aquariusRwAquaSAC, big.NewInt(7_000_000)),
		aquariusClaim(63_600_001, claimT.Add(time.Hour), "v", 1, aquariusRwUserB, aquariusRwAquaSAC, big.NewInt(3_000_000)),
		aquariusClaim(63_600_002, claimT.Add(2*time.Hour), "v", 2, aquariusRwUserB, aquariusRwUsdcSAC, big.NewInt(5)),
	} {
		if err := store.InsertAquariusRewardsEvent(ctx, e); err != nil {
			t.Fatalf("InsertAquariusRewardsEvent: %v", err)
		}
	}

	blk, err := store.BuildProtocolBespoke(ctx, "aquarius", "amm", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke = nil, want a block carrying the rewards drill-down")
	}
	kpis := kpiMap(blk)
	assertKPI(t, kpis, "Reward claims (30d)", "3")
	assertKPI(t, kpis, "Distinct claimants (30d)", "2")
	if v, ok := kpis["Reward volume (30d)"]; ok {
		t.Errorf("Reward volume (30d) = %q published across two reward tokens; base units of different tokens do not add", v)
	}

	var volRows [][]string
	for _, tb := range blk.Tables {
		if tb.Title == "Reward volume by token (30d)" {
			volRows = tb.Rows
		}
	}
	want := [][]string{
		{aquariusRwAquaSAC, "2", "10000000"},
		{aquariusRwUsdcSAC, "1", "5"},
	}
	if len(volRows) != len(want) {
		t.Fatalf("Reward volume by token (30d) rows = %v, want %v", volRows, want)
	}
	for i := range want {
		for c := range want[i] {
			if volRows[i][c] != want[i][c] {
				t.Errorf("Reward volume by token (30d) row %d = %v, want %v", i, volRows[i], want[i])
				break
			}
		}
	}
}

// aquariusClaim builds one claim_reward row as the decoder lands it:
// reward token in attributes.reward_token, claimant in user_address.
func aquariusClaim(ledger uint32, at time.Time, txSeed string, n int, user, rewardSAC string, amount *big.Int) timescale.AquariusRewardsEvent {
	return timescale.AquariusRewardsEvent{
		ContractID: aquariusRwPool, Ledger: ledger, LedgerCloseTime: at, TxHash: pad64(txSeed, n),
		Kind: timescale.AquariusRewardsClaimReward, UserAddress: user, Amount: amtPtr(canonical.NewAmount(amount)),
		Attributes: map[string]any{"reward_token": rewardSAC},
	}
}

// TestLatestAquariusAdminEvents exercises the governance-events reader
// (LatestAquariusAdminEvents): empty-safe, newest-first ordering
// (unwindowed, per the reader's doc — unlike the windowed bespoke augments),
// and kind/admin/target/ledger fields round-trip.
func TestLatestAquariusAdminEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if events, err := store.LatestAquariusAdminEvents(ctx, 25); err != nil {
		t.Fatalf("LatestAquariusAdminEvents (empty): %v", err)
	} else if len(events) != 0 {
		t.Fatalf("LatestAquariusAdminEvents (empty) = %+v, want none", events)
	}
	if total, err := store.AquariusAdminLifetimeTotal(ctx); err != nil {
		t.Fatalf("AquariusAdminLifetimeTotal (empty): %v", err)
	} else if total != 0 {
		t.Fatalf("AquariusAdminLifetimeTotal (empty) = %d, want 0", total)
	}

	base := time.Now().UTC().Add(-72 * time.Hour)
	admin := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	wasmHash := "d0d1b1e0f56d4c3a9b2e1f7c8a6d5e4b3c2a1908f7e6d5c4b3a291807f6e5d4c"

	events := []timescale.AquariusAdminEvent{
		{
			ContractID: aquariusRwRouter, Ledger: 61_000_000, LedgerCloseTime: base, TxHash: pad64("g", 0), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusAdminCommitUpgrade, Admin: admin, Target: wasmHash,
		},
		{
			ContractID: aquariusRwRouter, Ledger: 61_000_500, LedgerCloseTime: base.Add(time.Hour), TxHash: pad64("g", 1), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusAdminApplyUpgrade, Admin: admin, Target: wasmHash,
		},
		{
			ContractID: aquariusRwPool, Ledger: 61_001_000, LedgerCloseTime: base.Add(2 * time.Hour), TxHash: pad64("g", 2), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusAdminPoolGaugeSwitchToken, Target: aquariusRwUserA,
		},
	}
	for _, e := range events {
		if err := store.InsertAquariusAdminEvent(ctx, e); err != nil {
			t.Fatalf("InsertAquariusAdminEvent %s: %v", e.Kind, err)
		}
	}

	got, err := store.LatestAquariusAdminEvents(ctx, 25)
	if err != nil {
		t.Fatalf("LatestAquariusAdminEvents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("LatestAquariusAdminEvents = %d rows, want 3", len(got))
	}
	// Newest first: pool_gauge_switch_token (base+2h) leads.
	if got[0].Kind != timescale.AquariusAdminPoolGaugeSwitchToken || got[0].ContractID != aquariusRwPool {
		t.Errorf("got[0] = %+v, want the pool_gauge_switch_token row first", got[0])
	}
	if got[0].Admin != "" {
		t.Errorf("got[0].Admin = %q, want empty (kind carries none)", got[0].Admin)
	}
	if got[0].Target != aquariusRwUserA {
		t.Errorf("got[0].Target = %q, want %q", got[0].Target, aquariusRwUserA)
	}
	// Oldest last: commit_upgrade.
	last := got[len(got)-1]
	if last.Kind != timescale.AquariusAdminCommitUpgrade || last.Ledger != 61_000_000 {
		t.Errorf("got[last] = %+v, want the commit_upgrade row last", last)
	}
	if last.Admin != admin || last.Target != wasmHash {
		t.Errorf("got[last] admin/target = %q/%q, want %q/%q", last.Admin, last.Target, admin, wasmHash)
	}

	total, err := store.AquariusAdminLifetimeTotal(ctx)
	if err != nil {
		t.Fatalf("AquariusAdminLifetimeTotal: %v", err)
	}
	if total != 3 {
		t.Errorf("AquariusAdminLifetimeTotal = %d, want 3", total)
	}
}

// TestBespokeAquariusRewardsSurfaced proves the v0.12 "backfilled but
// served nowhere" gap (7.3M+ rows across aquarius_rewards_events +
// aquarius_admin) is closed: the Aquarius bespoke DEX block gains the
// rewards + governance KPIs/tables/series once either table carries data,
// stays empty-safe (nil block) when nothing has been captured, and the new
// KPIs sit ALONGSIDE (not replacing) the existing reserve-depth block from
// aquariusReserveBlocks. No frontend change is needed — BespokeSection
// renders KPIs/series/tables generically.
func TestBespokeAquariusRewardsSurfaced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Empty-safe: no trades, no reserves, no rewards/admin → nil block.
	blk, err := store.BuildProtocolBespoke(ctx, "aquarius", "amm", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke (empty): %v", err)
	}
	if blk != nil {
		t.Fatalf("BuildProtocolBespoke (empty) = %+v, want nil", blk)
	}

	// Seed a reserve snapshot (the pre-existing depth block) + rewards +
	// governance activity, so we can prove they coexist.
	if err := store.InsertAquariusReserves(ctx, timescale.AquariusReservesEvent{
		ContractID:      aquariusRwPool,
		Ledger:          63_500_000,
		LedgerCloseTime: time.Now().UTC().Add(-24 * time.Hour),
		TxHash:          pad64("r", 0),
		OpIndex:         0,
		EventIndex:      0,
		Reserves: []canonical.Amount{
			canonical.NewAmount(big.NewInt(123_456_789)),
			canonical.NewAmount(big.NewInt(987_654_321)),
		},
	}); err != nil {
		t.Fatalf("InsertAquariusReserves: %v", err)
	}

	claimT := time.Now().UTC().Add(-5 * 24 * time.Hour)
	if err := store.InsertAquariusRewardsEvent(ctx,
		aquariusClaim(63_500_100, claimT, "s", 0, aquariusRwUserA, aquariusRwAquaSAC, big.NewInt(42_000))); err != nil {
		t.Fatalf("InsertAquariusRewardsEvent: %v", err)
	}
	if err := store.InsertAquariusAdminEvent(ctx, timescale.AquariusAdminEvent{
		ContractID: aquariusRwRouter, Ledger: 63_500_050, LedgerCloseTime: claimT, TxHash: pad64("s", 1), OpIndex: 0, EventIndex: 0,
		Kind: timescale.AquariusAdminEnableEmergencyMode, Admin: aquariusRwUserB,
	}); err != nil {
		t.Fatalf("InsertAquariusAdminEvent: %v", err)
	}

	blk, err = store.BuildProtocolBespoke(ctx, "aquarius", "amm", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke = nil, want a block carrying reserves + rewards + governance")
	}

	kpis := kpiMap(blk)
	assertKPI(t, kpis, "Rewards-gauge events (lifetime)", "1")
	assertKPI(t, kpis, "Reward claims (30d)", "1")
	assertKPI(t, kpis, "Reward volume (30d)", "42000")
	assertKPI(t, kpis, "Distinct claimants (30d)", "1")
	assertKPI(t, kpis, "Governance events (lifetime)", "1")
	// The pre-existing reserve-depth KPI must still be present (additive, not replaced).
	if _, ok := kpis["Latest reserve snapshot"]; !ok {
		t.Errorf("aquarius block lost its existing reserve-depth KPI after adding rewards; KPIs=%+v", blk.KPIs)
	}

	var hasKindTable, hasGovTable, hasVolTable bool
	for _, tb := range blk.Tables {
		switch tb.Title {
		case "Reward volume by token (30d)":
			hasVolTable = true
			if len(tb.Rows) != 1 || tb.Rows[0][0] != aquariusRwAquaSAC || tb.Rows[0][2] != "42000" {
				t.Errorf("reward-volume-by-token table = %+v, want a single AQUA row of 42000", tb.Rows)
			}
		case "Rewards events by kind (lifetime)":
			hasKindTable = true
			if len(tb.Rows) != 1 || tb.Rows[0][0] != "claim_reward" {
				t.Errorf("rewards-by-kind table = %+v, want a single claim_reward row", tb.Rows)
			}
		case "Recent governance events":
			hasGovTable = true
			if len(tb.Rows) != 1 || tb.Rows[0][1] != "enable_emergency_mode" {
				t.Errorf("governance table = %+v, want a single enable_emergency_mode row", tb.Rows)
			}
		}
	}
	if !hasVolTable {
		t.Errorf("bespoke block missing 'Reward volume by token (30d)' table; Tables=%+v", blk.Tables)
	}
	if !hasKindTable {
		t.Errorf("bespoke block missing 'Rewards events by kind (lifetime)' table; Tables=%+v", blk.Tables)
	}
	if !hasGovTable {
		t.Errorf("bespoke block missing 'Recent governance events' table; Tables=%+v", blk.Tables)
	}

	var hasClaimSeries bool
	for _, s := range blk.Series {
		if s.Name == "Daily reward claims" {
			hasClaimSeries = true
			if len(s.Points) != 1 || s.Points[0].Value != "1" {
				t.Errorf("claim series points = %+v, want one point of value 1", s.Points)
			}
		}
	}
	if !hasClaimSeries {
		t.Errorf("bespoke block missing 'Daily reward claims' series; Series=%+v", blk.Series)
	}
}

// amtPtr returns a pointer to a canonical.Amount value, for the
// AquariusRewardsEvent.Amount *canonical.Amount optional field.
func amtPtr(a canonical.Amount) *canonical.Amount { return &a }

// TestCometLiquidityRoundTrip exercises the
// InsertCometLiquidity path through real TimescaleDB. Validates
// the migration-0042 schema (PK shape, NUMERIC i128 preservation,
// per-kind/direction CHECK constraints, withdraw-only
// pool_amount_in) by writing one row per kind and reading them
// back.
func TestCometLiquidityRoundTrip(t *testing.T) {
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
		// Blend's backstop Comet pool (per docs/operations/wasm-audits/comet.md).
		pool   = "CAS3FL6TLZKDGGSISDBWGGPXT3NRR4DYTZD7YOD3HMYO6LTJUVGRVEAM"
		caller = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAALI4"
		token1 = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC" // synthetic
		token2 = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
	)
	t0 := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)

	// ─── join_pool ───────────────────────────────────────────────
	join := timescale.CometLiquidityEvent{
		ContractID:      pool,
		Ledger:          1000,
		LedgerCloseTime: t0,
		TxHash:          "1100000000000000000000000000000000000000000000000000000000000001",
		OpIndex:         0,
		Kind:            timescale.CometLiquidityJoinPool,
		Caller:          caller,
		Token:           token1,
		Amount:          canonical.NewAmount(big.NewInt(1_500_000_000)),
	}
	if err := store.InsertCometLiquidity(ctx, join); err != nil {
		t.Fatalf("InsertCometLiquidity (join_pool): %v", err)
	}
	// Idempotent re-insert — ON CONFLICT DO NOTHING.
	if err := store.InsertCometLiquidity(ctx, join); err != nil {
		t.Fatalf("InsertCometLiquidity (join_pool dup): %v", err)
	}

	// Multi-token join: same (ledger, tx_hash, op_index) but
	// different `token` — must NOT collide thanks to `token` in PK.
	join2 := join
	join2.Token = token2
	join2.Amount = canonical.NewAmount(big.NewInt(600_000))
	if err := store.InsertCometLiquidity(ctx, join2); err != nil {
		t.Fatalf("InsertCometLiquidity (join_pool token2): %v", err)
	}

	// ─── exit_pool ───────────────────────────────────────────────
	if err := store.InsertCometLiquidity(ctx, timescale.CometLiquidityEvent{
		ContractID:      pool,
		Ledger:          1100,
		LedgerCloseTime: t0.Add(5 * time.Minute),
		TxHash:          "1100000000000000000000000000000000000000000000000000000000000002",
		OpIndex:         0,
		Kind:            timescale.CometLiquidityExitPool,
		Caller:          caller,
		Token:           token1,
		Amount:          canonical.NewAmount(big.NewInt(250_000_000)),
	}); err != nil {
		t.Fatalf("InsertCometLiquidity (exit_pool): %v", err)
	}

	// ─── deposit (single-asset) ──────────────────────────────────
	if err := store.InsertCometLiquidity(ctx, timescale.CometLiquidityEvent{
		ContractID:      pool,
		Ledger:          1200,
		LedgerCloseTime: t0.Add(10 * time.Minute),
		TxHash:          "1100000000000000000000000000000000000000000000000000000000000003",
		OpIndex:         0,
		Kind:            timescale.CometLiquidityDeposit,
		Caller:          caller,
		Token:           token1,
		Amount:          canonical.NewAmount(big.NewInt(100_000)),
	}); err != nil {
		t.Fatalf("InsertCometLiquidity (deposit): %v", err)
	}

	// ─── withdraw (carries pool_amount_in) ───────────────────────
	if err := store.InsertCometLiquidity(ctx, timescale.CometLiquidityEvent{
		ContractID:      pool,
		Ledger:          1300,
		LedgerCloseTime: t0.Add(15 * time.Minute),
		TxHash:          "1100000000000000000000000000000000000000000000000000000000000004",
		OpIndex:         0,
		Kind:            timescale.CometLiquidityWithdraw,
		Caller:          caller,
		Token:           token1,
		Amount:          canonical.NewAmount(big.NewInt(50_000)),
		PoolAmountIn:    canonical.NewAmount(big.NewInt(12_345)),
	}); err != nil {
		t.Fatalf("InsertCometLiquidity (withdraw): %v", err)
	}

	// ─── Read-back: per-kind row count + direction mapping ────────
	type rowCount struct {
		kind      string
		direction string
		n         int
	}
	rows, err := store.DB().QueryContext(ctx, `
		SELECT event_kind, direction, COUNT(*) AS n
		FROM comet_liquidity
		WHERE contract_id = $1
		GROUP BY event_kind, direction
		ORDER BY event_kind`, pool)
	if err != nil {
		t.Fatalf("read-back query: %v", err)
	}
	defer rows.Close()
	got := map[string]rowCount{}
	for rows.Next() {
		var rc rowCount
		if err := rows.Scan(&rc.kind, &rc.direction, &rc.n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[rc.kind] = rc
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	want := map[string]rowCount{
		"join_pool": {kind: "join_pool", direction: "add", n: 2}, // two tokens on same join
		"exit_pool": {kind: "exit_pool", direction: "remove", n: 1},
		"deposit":   {kind: "deposit", direction: "add", n: 1},
		"withdraw":  {kind: "withdraw", direction: "remove", n: 1},
	}
	for kind, w := range want {
		g, ok := got[kind]
		if !ok {
			t.Errorf("missing kind=%q in result", kind)
			continue
		}
		if g.direction != w.direction {
			t.Errorf("kind=%q direction = %q, want %q", kind, g.direction, w.direction)
		}
		if g.n != w.n {
			t.Errorf("kind=%q count = %d, want %d", kind, g.n, w.n)
		}
	}

	// ─── pool_amount_in is NULL on non-withdraw rows ─────────────
	var nonWithdrawWithPoolAmt int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM comet_liquidity
		WHERE contract_id = $1
		  AND event_kind <> 'withdraw'
		  AND pool_amount_in IS NOT NULL`, pool).Scan(&nonWithdrawWithPoolAmt); err != nil {
		t.Fatalf("pool_amount_in NULL check: %v", err)
	}
	if nonWithdrawWithPoolAmt != 0 {
		t.Errorf("found %d non-withdraw rows with non-NULL pool_amount_in; want 0", nonWithdrawWithPoolAmt)
	}

	// ─── pool_amount_in round-trips correctly on withdraw ────────
	var poolAmt canonical.Amount
	if err := store.DB().QueryRowContext(ctx, `
		SELECT pool_amount_in::text FROM comet_liquidity
		WHERE contract_id = $1 AND event_kind = 'withdraw'
		LIMIT 1`, pool).Scan(&poolAmt); err != nil {
		t.Fatalf("read pool_amount_in: %v", err)
	}
	if poolAmt.BigInt().Int64() != 12_345 {
		t.Errorf("pool_amount_in = %s, want 12345", poolAmt)
	}
}

// TestCometLiquidity_LargeI128 verifies the NUMERIC column preserves
// 128-bit values — per ADR-0003 a Soroban i128 must never be silently
// truncated. A multi-billion-share LP join is the realistic worst
// case (single XLM unit at the contract's own precision can already
// exceed int64).
func TestCometLiquidity_LargeI128(t *testing.T) {
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
		pool   = "CAS3FL6TLZKDGGSISDBWGGPXT3NRR4DYTZD7YOD3HMYO6LTJUVGRVEAM"
		caller = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAALI4"
		token  = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	)
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)

	if err := store.InsertCometLiquidity(ctx, timescale.CometLiquidityEvent{
		ContractID:      pool,
		Ledger:          5000,
		LedgerCloseTime: time.Now().UTC(),
		TxHash:          "2200000000000000000000000000000000000000000000000000000000000001",
		OpIndex:         0,
		Kind:            timescale.CometLiquidityJoinPool,
		Caller:          caller,
		Token:           token,
		Amount:          canonical.NewAmount(huge),
	}); err != nil {
		t.Fatalf("InsertCometLiquidity (huge): %v", err)
	}

	var amt canonical.Amount
	if err := store.DB().QueryRowContext(ctx, `
		SELECT amount::text FROM comet_liquidity
		WHERE contract_id = $1 LIMIT 1`, pool).Scan(&amt); err != nil {
		t.Fatalf("read amount: %v", err)
	}
	if amt.BigInt().Cmp(huge) != 0 {
		t.Errorf("got %s, want %s — i128 / NUMERIC round-trip lost precision", amt, huge)
	}
}

// Reused public strkeys (contract / token ids — public identifiers, not
// secrets) for the DEX liquidity-depth fixtures.
const (
	dexCometPool     = "CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M"
	dexCometToken    = "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH"
	dexPhoenixPool   = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"
	dexTokenA        = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"
	dexTokenB        = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY"
	dexStakeContract = "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG"
	dexLPToken       = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"
)

// TestLatestCometLiquidityFlowsAndBespoke exercises the Comet liquidity-depth
// READ side (LatestCometLiquidityFlows) + the bespokeDEX comet augment.
// Proves empty-safe, net-flow (added − removed) with i128/NUMERIC
// preservation, and — critically — that the surfaced depth carries the
// contract-identity caveat (Comet is gated on contract identity; rows
// captured before the gate predate it and were not re-verified).
func TestLatestCometLiquidityFlowsAndBespoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if flows, err := store.LatestCometLiquidityFlows(ctx, 90); err != nil {
		t.Fatalf("LatestCometLiquidityFlows (empty): %v", err)
	} else if flows != nil {
		t.Fatalf("LatestCometLiquidityFlows (empty) = %+v, want nil", flows)
	}

	base := time.Now().UTC().Add(-12 * time.Hour)
	addHuge, _ := new(big.Int).SetString("10000000000000000000000", 10) // > 2^63
	removed := big.NewInt(300)
	wantNet := new(big.Int).Sub(addHuge, removed)

	if err := store.InsertCometLiquidity(ctx, timescale.CometLiquidityEvent{
		ContractID: dexCometPool, Ledger: 61_000_000, LedgerCloseTime: base, TxHash: pad64("e", 0), OpIndex: 0, EventIndex: 0,
		Kind: timescale.CometLiquidityJoinPool, Caller: credOwnerA, Token: dexCometToken,
		Amount: canonical.NewAmount(addHuge),
	}); err != nil {
		t.Fatalf("InsertCometLiquidity join_pool: %v", err)
	}
	if err := store.InsertCometLiquidity(ctx, timescale.CometLiquidityEvent{
		ContractID: dexCometPool, Ledger: 61_000_001, LedgerCloseTime: base.Add(time.Minute), TxHash: pad64("e", 1), OpIndex: 0, EventIndex: 0,
		Kind: timescale.CometLiquidityWithdraw, Caller: credOwnerA, Token: dexCometToken,
		Amount: canonical.NewAmount(removed), PoolAmountIn: canonical.NewAmount(big.NewInt(50)),
	}); err != nil {
		t.Fatalf("InsertCometLiquidity withdraw: %v", err)
	}

	flows, err := store.LatestCometLiquidityFlows(ctx, 90)
	if err != nil {
		t.Fatalf("LatestCometLiquidityFlows: %v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("flows = %d, want 1", len(flows))
	}
	f := flows[0]
	if f.Added.BigInt().Cmp(addHuge) != 0 {
		t.Errorf("Added = %s, want %s — i128/NUMERIC lost precision", f.Added, addHuge)
	}
	if f.Removed.BigInt().Cmp(removed) != 0 {
		t.Errorf("Removed = %s, want %s", f.Removed, removed)
	}
	if f.Net.BigInt().Cmp(wantNet) != 0 {
		t.Errorf("Net = %s, want %s", f.Net, wantNet)
	}
	if f.Events != 2 {
		t.Errorf("Events = %d, want 2", f.Events)
	}

	blk, err := store.BuildProtocolBespoke(ctx, "comet", "amm", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke comet: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke comet = nil, want a block carrying the liquidity-depth KPI")
	}
	kpis := kpiMap(blk)
	assertKPI(t, kpis, "Pools with LP activity (90d)", "1")
	assertKPI(t, kpis, "LP events (90d)", "2")
	if !hasTable(blk, "Net liquidity flow by pool/token (window)") {
		t.Errorf("comet block missing net-flow table; Tables=%+v", blk.Tables)
	}
	if !anyNote(blk, "CS-026") {
		t.Errorf("comet block missing the CS-026 gating caveat note; Notes=%+v", blk.Notes)
	}
}

// TestLatestPhoenixLiquidityFlowsAndBespoke exercises the Phoenix
// liquidity-depth + LP-staking READ sides (LatestPhoenixLiquidityFlows +
// PhoenixStakeWindowStats) and the bespokeDEX phoenix augment. Proves
// empty-safe, two-token net flow with positional token resolution from the
// most recent provide, i128/NUMERIC preservation, and the staking KPI.
func TestLatestPhoenixLiquidityFlowsAndBespoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if flows, err := store.LatestPhoenixLiquidityFlows(ctx, 90); err != nil {
		t.Fatalf("LatestPhoenixLiquidityFlows (empty): %v", err)
	} else if flows != nil {
		t.Fatalf("LatestPhoenixLiquidityFlows (empty) = %+v, want nil", flows)
	}
	if st, err := store.PhoenixStakeWindowStats(ctx, 90); err != nil {
		t.Fatalf("PhoenixStakeWindowStats (empty): %v", err)
	} else if st != nil {
		t.Fatalf("PhoenixStakeWindowStats (empty) = %+v, want nil", st)
	}

	base := time.Now().UTC().Add(-8 * time.Hour)
	provA, _ := new(big.Int).SetString("55555555555555555555", 10) // > 2^63
	provB := big.NewInt(2_000_000)
	wdA := big.NewInt(1_000_000)
	wdB := big.NewInt(500_000)
	wantNetA := new(big.Int).Sub(provA, wdA)
	wantNetB := new(big.Int).Sub(provB, wdB)

	// Provide carries token addresses; withdraw omits them (resolved
	// positionally from the most recent provide).
	if err := store.InsertPhoenixLiquidityChange(ctx, timescale.PhoenixLiquidityChange{
		Pool: dexPhoenixPool, Ledger: 60_000_000, ObservedAt: base, TxHash: pad64("f", 0), OpIndex: 0, EventIndex: 0,
		Action: timescale.PhoenixProvideLiquidity, Sender: credOwnerA,
		TokenA: dexTokenA, TokenB: dexTokenB, AmountA: provA.String(), AmountB: provB.String(),
	}); err != nil {
		t.Fatalf("InsertPhoenixLiquidityChange provide: %v", err)
	}
	if err := store.InsertPhoenixLiquidityChange(ctx, timescale.PhoenixLiquidityChange{
		Pool: dexPhoenixPool, Ledger: 60_000_001, ObservedAt: base.Add(time.Minute), TxHash: pad64("f", 1), OpIndex: 0, EventIndex: 0,
		Action: timescale.PhoenixWithdrawLiquidity, Sender: credOwnerA,
		AmountA: wdA.String(), AmountB: wdB.String(), SharesAmount: "123456",
	}); err != nil {
		t.Fatalf("InsertPhoenixLiquidityChange withdraw: %v", err)
	}

	flows, err := store.LatestPhoenixLiquidityFlows(ctx, 90)
	if err != nil {
		t.Fatalf("LatestPhoenixLiquidityFlows: %v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("flows = %d, want 1", len(flows))
	}
	pf := flows[0]
	if pf.TokenA != dexTokenA || pf.TokenB != dexTokenB {
		t.Errorf("tokens = (%q,%q), want (%q,%q) — positional resolution from provide failed", pf.TokenA, pf.TokenB, dexTokenA, dexTokenB)
	}
	if pf.NetA.BigInt().Cmp(wantNetA) != 0 {
		t.Errorf("NetA = %s, want %s — i128/NUMERIC lost precision", pf.NetA, wantNetA)
	}
	if pf.NetB.BigInt().Cmp(wantNetB) != 0 {
		t.Errorf("NetB = %s, want %s", pf.NetB, wantNetB)
	}
	if pf.Provides != 1 || pf.Withdraws != 1 {
		t.Errorf("provides/withdraws = %d/%d, want 1/1", pf.Provides, pf.Withdraws)
	}

	// LP staking.
	bondHuge, _ := new(big.Int).SetString("77777777777777777777", 10)
	unbond := big.NewInt(11_000_000)
	if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
		StakeContract: dexStakeContract, Ledger: 60_100_000, ObservedAt: base.Add(2 * time.Minute), TxHash: pad64("g", 0), OpIndex: 0, EventIndex: 0,
		Action: timescale.PhoenixBond, User: credOwnerA, LPToken: dexLPToken, Amount: bondHuge.String(),
	}); err != nil {
		t.Fatalf("InsertPhoenixStakeEvent bond: %v", err)
	}
	if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
		StakeContract: dexStakeContract, Ledger: 60_100_001, ObservedAt: base.Add(3 * time.Minute), TxHash: pad64("g", 1), OpIndex: 0, EventIndex: 0,
		Action: timescale.PhoenixUnbond, User: credSettler, LPToken: dexLPToken, Amount: unbond.String(),
	}); err != nil {
		t.Fatalf("InsertPhoenixStakeEvent unbond: %v", err)
	}
	stk, err := store.PhoenixStakeWindowStats(ctx, 90)
	if err != nil {
		t.Fatalf("PhoenixStakeWindowStats: %v", err)
	}
	if stk == nil {
		t.Fatal("PhoenixStakeWindowStats = nil, want summary")
	}
	if stk.Bonded.BigInt().Cmp(bondHuge) != 0 {
		t.Errorf("Bonded = %s, want %s — i128/NUMERIC lost precision", stk.Bonded, bondHuge)
	}
	if stk.UniqueStakers != 2 {
		t.Errorf("UniqueStakers = %d, want 2", stk.UniqueStakers)
	}

	blk, err := store.BuildProtocolBespoke(ctx, "phoenix", "amm", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke phoenix: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke phoenix = nil, want a block")
	}
	kpis := kpiMap(blk)
	assertKPI(t, kpis, "Pools with LP activity (90d)", "1")
	assertKPI(t, kpis, "LP staked (90d)", bondHuge.String())
	if !hasTable(blk, "Net liquidity flow by pool (window)") {
		t.Errorf("phoenix block missing net-flow table; Tables=%+v", blk.Tables)
	}
}

// TestSoroswapSkimAndBespoke exercises the Soroswap skim READ side
// (SoroswapSkimWindowStats) + the bespokeDEX soroswap augment. Proves
// empty-safe, i128/NUMERIC preservation of the summed excess amounts, and
// that a skim-only source (no trades in the window) still surfaces a block.
func TestSoroswapSkimAndBespoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if sk, err := store.SoroswapSkimWindowStats(ctx, 90); err != nil {
		t.Fatalf("SoroswapSkimWindowStats (empty): %v", err)
	} else if sk != nil {
		t.Fatalf("SoroswapSkimWindowStats (empty) = %+v, want nil", sk)
	}

	base := time.Now().UTC().Add(-6 * time.Hour)
	amt0Huge, _ := new(big.Int).SetString("33333333333333333333", 10) // > 2^63
	wantAmt0 := new(big.Int).Add(amt0Huge, big.NewInt(50))
	tx0 := make([]byte, 32)
	tx0[31] = 1
	tx1 := make([]byte, 32)
	tx1[31] = 2

	if err := store.InsertSoroswapSkimEvent(ctx, timescale.SoroswapSkimEvent{
		ContractID: dexCometToken, Ledger: 59_000_000, LedgerCloseTime: base, TxHash: tx0, OpIndex: 0, EventIndex: 0,
		Amount0: amt0Huge.String(), Amount1: "100",
	}); err != nil {
		t.Fatalf("InsertSoroswapSkimEvent 0: %v", err)
	}
	if err := store.InsertSoroswapSkimEvent(ctx, timescale.SoroswapSkimEvent{
		ContractID: dexCometToken, Ledger: 59_000_001, LedgerCloseTime: base.Add(time.Minute), TxHash: tx1, OpIndex: 0, EventIndex: 0,
		Amount0: "50", Amount1: "200",
	}); err != nil {
		t.Fatalf("InsertSoroswapSkimEvent 1: %v", err)
	}

	sk, err := store.SoroswapSkimWindowStats(ctx, 90)
	if err != nil {
		t.Fatalf("SoroswapSkimWindowStats: %v", err)
	}
	if sk == nil {
		t.Fatal("SoroswapSkimWindowStats = nil, want summary")
	}
	if sk.Skims != 2 {
		t.Errorf("Skims = %d, want 2", sk.Skims)
	}
	if sk.Pairs != 1 || len(sk.ByPair) != 1 {
		t.Fatalf("Pairs = %d, ByPair = %d rows, want 1 / 1", sk.Pairs, len(sk.ByPair))
	}
	if got := sk.ByPair[0].Amount0; got.BigInt().Cmp(wantAmt0) != 0 {
		t.Errorf("ByPair[0].Amount0 = %s, want %s — i128/NUMERIC lost precision", got, wantAmt0)
	}

	blk, err := store.BuildProtocolBespoke(ctx, "soroswap", "amm", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke soroswap: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke soroswap = nil, want a block carrying the skim KPI")
	}
	kpis := kpiMap(blk)
	assertKPI(t, kpis, "Skim events (90d)", "2")
	rows := skimTableByPair(t, blk)
	if got := rows[dexCometToken]; len(got) != 6 || got[3] != wantAmt0.String() || got[5] != "300" {
		t.Errorf("Skims by pair[%s] = %v, want token0 %s / token1 300", dexCometToken, got, wantAmt0)
	}
}

// TestSoroswapSkimBespokeNeverSumsAcrossPairs pins that the served Soroswap
// block keeps skim amounts per pair. token0 of one pair and token0 of another
// are different tokens with different decimals; adding their base units into
// one "Skimmed token0" figure publishes a quantity of no currency at all.
func TestSoroswapSkimBespokeNeverSumsAcrossPairs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Pair A wraps a 7-decimal token; pair B an 18-decimal one.
	pairA, pairB := dexCometPool, dexPhoenixPool
	if err := store.UpsertSoroswapPair(ctx, pairA, dexTokenA, dexTokenB); err != nil {
		t.Fatalf("UpsertSoroswapPair A: %v", err)
	}
	if err := store.UpsertSoroswapPair(ctx, pairB, dexLPToken, dexTokenA); err != nil {
		t.Fatalf("UpsertSoroswapPair B: %v", err)
	}
	base := time.Now().UTC().Add(-6 * time.Hour)
	for i, e := range []timescale.SoroswapSkimEvent{
		{ContractID: pairA, Amount0: "10000000", Amount1: "7"},
		{ContractID: pairB, Amount0: "500000000000000000", Amount1: "3"},
	} {
		tx := make([]byte, 32)
		tx[31] = byte(10 + i)
		e.Ledger, e.LedgerCloseTime, e.TxHash = uint32(59_100_000+i), base.Add(time.Duration(i)*time.Minute), tx
		if err := store.InsertSoroswapSkimEvent(ctx, e); err != nil {
			t.Fatalf("InsertSoroswapSkimEvent %d: %v", i, err)
		}
	}

	blk, err := store.BuildProtocolBespoke(ctx, "soroswap", "amm", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke soroswap: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke soroswap = nil, want a block carrying the skim KPIs")
	}
	kpis := kpiMap(blk)
	assertKPI(t, kpis, "Skim events (90d)", "2")
	assertKPI(t, kpis, "Pairs skimmed (90d)", "2")
	for _, k := range blk.KPIs {
		if k.Value == "500000000010000000" || k.Value == "10" {
			t.Errorf("KPI %q = %q is a cross-pair sum of different tokens' base units", k.Label, k.Value)
		}
	}

	rows := skimTableByPair(t, blk)
	want := map[string][]string{
		pairA: {pairA, "1", dexTokenA, "10000000", dexTokenB, "7"},
		pairB: {pairB, "1", dexLPToken, "500000000000000000", dexTokenA, "3"},
	}
	if len(rows) != len(want) {
		t.Errorf("Skims by pair has %d rows, want %d: %v", len(rows), len(want), rows)
	}
	for pair, w := range want {
		if got := rows[pair]; strings.Join(got, "|") != strings.Join(w, "|") {
			t.Errorf("Skims by pair[%s] = %v, want %v", pair, got, w)
		}
	}
}

// skimTableByPair returns the "Skims by pair" table rows keyed by pair id,
// failing the test when the table is absent.
func skimTableByPair(t *testing.T, blk *timescale.BespokeBlock) map[string][]string {
	t.Helper()
	for _, tb := range blk.Tables {
		if tb.Title != "Skims by pair" {
			continue
		}
		out := map[string][]string{}
		for _, r := range tb.Rows {
			out[r[0]] = r
		}
		return out
	}
	t.Fatal(`Soroswap block has no "Skims by pair" table`)
	return nil
}

// kpiMap indexes a bespoke block's KPIs by label.
func kpiMap(blk *timescale.BespokeBlock) map[string]string {
	m := map[string]string{}
	for _, k := range blk.KPIs {
		m[k.Label] = k.Value
	}
	return m
}

// hasTable reports whether the block carries a table with the given title.
func hasTable(blk *timescale.BespokeBlock, title string) bool {
	for _, tb := range blk.Tables {
		if tb.Title == title {
			return true
		}
	}
	return false
}

// anyNote reports whether any block note contains sub.
func anyNote(blk *timescale.BespokeBlock, sub string) bool {
	for _, n := range blk.Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// TestPhoenixLiquidityRoundTrip exercises InsertPhoenixLiquidityChange
// across both provide_liquidity (token addresses populated, shares
// NULL) and withdraw_liquidity (token addresses NULL, shares
// populated) shapes through real TimescaleDB. Validates the
// migration 0044 PK, the per-column NULL-on-mismatch behaviour, the
// idempotent ON CONFLICT DO NOTHING semantics, and that a large
// i128 amount round-trips through NUMERIC without precision loss
// (ADR-0003).
func TestPhoenixLiquidityRoundTrip(t *testing.T) {
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
		pool   = "CDPHXPOOL00000000000000000000000000000000000000000000A"
		sender = "GPHXSENDER0000000000000000000000000000000000000000000B"
		tokenA = "CDTKNA000000000000000000000000000000000000000000000C"
		tokenB = "CDTKNB000000000000000000000000000000000000000000000D"
		largeI = "123456789012345678901234567890" // > 2^63
	)
	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	// ─── Provide_liquidity row ──────────────────────────────────
	provide := timescale.PhoenixLiquidityChange{
		Pool:       pool,
		Ledger:     62_500_000,
		ObservedAt: t0,
		TxHash:     "3300000000000000000000000000000000000000000000000000000000000001",
		OpIndex:    0,
		Action:     timescale.PhoenixProvideLiquidity,
		Sender:     sender,
		TokenA:     tokenA,
		TokenB:     tokenB,
		AmountA:    "1000000000",
		AmountB:    "50000000",
		// SharesAmount intentionally empty — provide rows don't carry it.
	}
	if err := store.InsertPhoenixLiquidityChange(ctx, provide); err != nil {
		t.Fatalf("InsertPhoenixLiquidityChange (provide): %v", err)
	}

	// Idempotent re-insert — same PK is a no-op.
	if err := store.InsertPhoenixLiquidityChange(ctx, provide); err != nil {
		t.Fatalf("InsertPhoenixLiquidityChange (provide dup): %v", err)
	}

	// ─── Withdraw_liquidity row at the same pool / later ledger ─
	withdraw := timescale.PhoenixLiquidityChange{
		Pool:       pool,
		Ledger:     62_500_100,
		ObservedAt: t0.Add(time.Hour),
		TxHash:     "3300000000000000000000000000000000000000000000000000000000000002",
		OpIndex:    0,
		Action:     timescale.PhoenixWithdrawLiquidity,
		Sender:     sender,
		// TokenA / TokenB intentionally empty — withdraw doesn't carry them.
		AmountA:      "990000000",
		AmountB:      "49000000",
		SharesAmount: "7000000",
	}
	if err := store.InsertPhoenixLiquidityChange(ctx, withdraw); err != nil {
		t.Fatalf("InsertPhoenixLiquidityChange (withdraw): %v", err)
	}

	// ─── Large-i128 row to prove NUMERIC round-trips ────────────
	huge := timescale.PhoenixLiquidityChange{
		Pool:       pool,
		Ledger:     62_500_200,
		ObservedAt: t0.Add(2 * time.Hour),
		TxHash:     "3300000000000000000000000000000000000000000000000000000000000003",
		OpIndex:    0,
		Action:     timescale.PhoenixProvideLiquidity,
		Sender:     sender,
		TokenA:     tokenA,
		TokenB:     tokenB,
		AmountA:    largeI,
		AmountB:    largeI,
	}
	if err := store.InsertPhoenixLiquidityChange(ctx, huge); err != nil {
		t.Fatalf("InsertPhoenixLiquidityChange (huge): %v", err)
	}

	// ─── Verify shape — query directly via the store's DB handle ─
	type row struct {
		action       string
		tokenA       *string
		tokenB       *string
		amountA      string
		amountB      string
		sharesAmount *string
	}
	var rows []row
	q := `
        SELECT action, token_a, token_b, amount_a::text, amount_b::text, shares_amount::text
          FROM phoenix_liquidity
         WHERE pool = $1
         ORDER BY ledger
    `
	dbh := store.DB()
	rs, err := dbh.QueryContext(ctx, q, pool)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rs.Close()
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.action, &r.tokenA, &r.tokenB, &r.amountA, &r.amountB, &r.sharesAmount); err != nil {
			t.Fatalf("scan: %v", err)
		}
		rows = append(rows, r)
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows (after dup-insert), want 3", len(rows))
	}

	// Provide row: tokens populated, shares NULL.
	if rows[0].action != "provide_liquidity" {
		t.Errorf("rows[0].action = %q", rows[0].action)
	}
	if rows[0].tokenA == nil || *rows[0].tokenA != tokenA {
		t.Errorf("rows[0].tokenA = %v, want %q", rows[0].tokenA, tokenA)
	}
	if rows[0].sharesAmount != nil {
		t.Errorf("rows[0].sharesAmount = %v, want NULL", *rows[0].sharesAmount)
	}

	// Withdraw row: tokens NULL, shares populated.
	if rows[1].action != "withdraw_liquidity" {
		t.Errorf("rows[1].action = %q", rows[1].action)
	}
	if rows[1].tokenA != nil {
		t.Errorf("rows[1].tokenA = %v, want NULL (withdraw doesn't emit token addresses)", *rows[1].tokenA)
	}
	if rows[1].sharesAmount == nil || *rows[1].sharesAmount != "7000000" {
		t.Errorf("rows[1].sharesAmount = %v, want 7000000", rows[1].sharesAmount)
	}

	// Large-i128 row: amount round-trips exactly.
	if rows[2].amountA != largeI {
		t.Errorf("rows[2].amountA = %q, want %q (NUMERIC precision lost?)", rows[2].amountA, largeI)
	}
}

// TestPhoenixStakeEventsRoundTrip exercises InsertPhoenixStakeEvent
// across bond + unbond with a shared user/contract/lp_token. The
// action column discriminator distinguishes the two directions; the
// PK keeps a same-(ledger, tx, op) bond+unbond pair from colliding.
func TestPhoenixStakeEventsRoundTrip(t *testing.T) {
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
		stakeContract = "CDSTAKE0000000000000000000000000000000000000000000000A"
		user          = "GUSER000000000000000000000000000000000000000000000000B"
		lpToken       = "CDLPTOK0000000000000000000000000000000000000000000000C"
	)
	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
		StakeContract: stakeContract,
		Ledger:        62_500_300,
		ObservedAt:    t0,
		TxHash:        "4400000000000000000000000000000000000000000000000000000000000001",
		OpIndex:       0,
		Action:        timescale.PhoenixBond,
		User:          user,
		LPToken:       lpToken,
		Amount:        "1000000",
	}); err != nil {
		t.Fatalf("InsertPhoenixStakeEvent (bond): %v", err)
	}

	if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
		StakeContract: stakeContract,
		Ledger:        62_500_400,
		ObservedAt:    t0.Add(time.Hour),
		TxHash:        "4400000000000000000000000000000000000000000000000000000000000002",
		OpIndex:       0,
		Action:        timescale.PhoenixUnbond,
		User:          user,
		LPToken:       lpToken,
		Amount:        "400000",
	}); err != nil {
		t.Fatalf("InsertPhoenixStakeEvent (unbond): %v", err)
	}

	// Same (ledger, tx, op) for a bond+unbond pair: distinct rows.
	const sharedTx = "4400000000000000000000000000000000000000000000000000000000000003"
	for _, action := range []timescale.PhoenixStakeAction{timescale.PhoenixBond, timescale.PhoenixUnbond} {
		if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
			StakeContract: stakeContract,
			Ledger:        62_500_500,
			ObservedAt:    t0.Add(2 * time.Hour),
			TxHash:        sharedTx,
			OpIndex:       0,
			Action:        action,
			User:          user,
			LPToken:       lpToken,
			Amount:        "1",
		}); err != nil {
			t.Fatalf("InsertPhoenixStakeEvent (%s @ shared tx): %v", action, err)
		}
	}

	// ─── Verify: 4 rows total ────────────────────────────────────
	var count int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM phoenix_stake_events WHERE stake_contract = $1`,
		stakeContract).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 4 {
		t.Errorf("count = %d, want 4 (bond/unbond pair at shared tx not colliding)", count)
	}

	// ─── Per-action breakdown ────────────────────────────────────
	type tally struct {
		action string
		n      int
		sum    string
	}
	var ts []tally
	rs, err := store.DB().QueryContext(ctx, `
        SELECT action, count(*), sum(amount)::text
          FROM phoenix_stake_events
         WHERE stake_contract = $1
         GROUP BY action
         ORDER BY action
    `, stakeContract)
	if err != nil {
		t.Fatalf("group query: %v", err)
	}
	defer rs.Close()
	for rs.Next() {
		var ti tally
		if err := rs.Scan(&ti.action, &ti.n, &ti.sum); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ts = append(ts, ti)
	}
	if len(ts) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(ts))
	}
	// bond: 1000000 + 1 = 1000001
	// unbond: 400000 + 1 = 400001
	if ts[0].action != "bond" || ts[0].n != 2 || ts[0].sum != "1000001" {
		t.Errorf("bond tally = %+v, want {bond 2 1000001}", ts[0])
	}
	if ts[1].action != "unbond" || ts[1].n != 2 || ts[1].sum != "400001" {
		t.Errorf("unbond tally = %+v, want {unbond 2 400001}", ts[1])
	}
}

// TestPhoenixLifecycleAndConfigEventsRoundTrip lands every action
// migration 0195 admits: a create_distribution_flow row with no user, the
// migration_* rows with no token, and the factory / blend-pool admin rows,
// with the i128 minimum-trading value round-tripping through NUMERIC.
func TestPhoenixLifecycleAndConfigEventsRoundTrip(t *testing.T) {
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
		stakeContract = "CDSTAKE0000000000000000000000000000000000000000000000A"
		user          = "GUSER000000000000000000000000000000000000000000000000B"
		asset         = "CDASSET0000000000000000000000000000000000000000000000C"
		pool          = "CDPHXPOOL00000000000000000000000000000000000000000000A"
		maxI128       = "170141183460469231731687303715884105727"
		tx            = "5500000000000000000000000000000000000000000000000000000000000001"
	)
	t0 := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	stake := []timescale.PhoenixStakeEvent{
		{Action: timescale.PhoenixCreateDistributionFlow, LPToken: asset},
		{Action: timescale.PhoenixMigrationStarted, User: user},
		{Action: timescale.PhoenixMigrationQueried, User: user},
		{Action: timescale.PhoenixMigrationCompleted, User: user},
	}
	for i, e := range stake {
		e.StakeContract, e.Ledger, e.ObservedAt, e.TxHash, e.EventIndex = stakeContract, 63_000_000, t0, tx, uint32(i) //nolint:gosec // small test index
		if err := store.InsertPhoenixStakeEvent(ctx, e); err != nil {
			t.Fatalf("InsertPhoenixStakeEvent (%s): %v", e.Action, err)
		}
	}
	if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
		StakeContract: stakeContract, Ledger: 63_000_000, ObservedAt: t0, TxHash: tx, EventIndex: 9,
		Action: timescale.PhoenixMigrationStarted,
	}); err == nil {
		t.Error("a migration_started row with no user was accepted")
	}

	rs, err := store.DB().QueryContext(ctx, `
        SELECT action, user_addr, lp_token, amount::text
          FROM phoenix_stake_events
         WHERE stake_contract = $1
         ORDER BY event_index`, stakeContract)
	if err != nil {
		t.Fatalf("stake query: %v", err)
	}
	defer rs.Close()
	n := 0
	for rs.Next() {
		var action string
		var userAddr, lpToken, amount *string
		if err := rs.Scan(&action, &userAddr, &lpToken, &amount); err != nil {
			t.Fatalf("scan: %v", err)
		}
		want := stake[n]
		if action != string(want.Action) || (userAddr != nil) != (want.User != "") ||
			(lpToken != nil) != (want.LPToken != "") || amount != nil {
			t.Errorf("row %d = %s user=%v lp_token=%v amount=%v, want %+v", n, action, userAddr, lpToken, amount, want)
		}
		n++
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if n != len(stake) {
		t.Fatalf("got %d stake rows, want %d", n, len(stake))
	}

	admin := []timescale.PhoenixAdminEvent{
		{Pool: pool, AdminAction: "factory_config_updated"},
		{Pool: pool, AdminAction: "blend_set_delegate", Admin: asset},
		{Pool: pool, AdminAction: "blend_set_min_trading_a", Value: "0"},
		{Pool: pool, AdminAction: "blend_set_min_trading_b", Value: maxI128},
	}
	for i, e := range admin {
		e.Ledger, e.LedgerCloseTime, e.TxHash, e.EventIndex = 63_000_001, t0, tx, uint32(i) //nolint:gosec // small test index
		if err := store.InsertPhoenixAdmin(ctx, e); err != nil {
			t.Fatalf("InsertPhoenixAdmin (%s): %v", e.AdminAction, err)
		}
	}
	ar, err := store.DB().QueryContext(ctx, `
        SELECT admin_action, admin, value::text
          FROM phoenix_admin_events
         WHERE pool = $1
         ORDER BY event_index`, pool)
	if err != nil {
		t.Fatalf("admin query: %v", err)
	}
	defer ar.Close()
	n = 0
	for ar.Next() {
		var action string
		var adminAddr, value *string
		if err := ar.Scan(&action, &adminAddr, &value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		want := admin[n]
		gotValue := ""
		if value != nil {
			gotValue = *value
		}
		if action != want.AdminAction || (adminAddr != nil) != (want.Admin != "") || gotValue != want.Value {
			t.Errorf("row %d = %s admin=%v value=%q, want %+v", n, action, adminAddr, gotValue, want)
		}
		n++
	}
	if err := ar.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if n != len(admin) {
		t.Fatalf("got %d admin rows, want %d", n, len(admin))
	}
}

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

	pairA := mustContractStrkey(t, 0xC0)
	pairB := mustContractStrkey(t, 0xC1)
	tok0 := mustContractStrkey(t, 0xC2)
	tok1 := mustContractStrkey(t, 0xC3)
	tok2 := mustContractStrkey(t, 0xC4)

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

	pairA := mustContractStrkey(t, 0xD0)
	pairB := mustContractStrkey(t, 0xD1)
	tok0 := mustContractStrkey(t, 0xD2)
	tok1 := mustContractStrkey(t, 0xD3)

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

// TestSoroswapSkimEvent_Insert exercises the InsertSoroswapSkimEvent
// path against real TimescaleDB. Migration 0042. Inserts the two
// shapes the decoder emits in production:
//
//  1. Initial shape: amounts present, `to_address` NULL.
//  2. Future-upgrade shape: amounts present, `to_address` populated.
//
// Asserts ON CONFLICT DO NOTHING idempotency (replay-safety) and
// that the NUMERIC columns round-trip an above-int64 i128 (ADR-0003).
func TestSoroswapSkimEvent_Insert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	pair := mustContractStrkey(t, 0xA0)
	to := mustContractStrkey(t, 0xA1)
	closedAt := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	txHash := make([]byte, 32)
	for i := range txHash {
		txHash[i] = 0xBE
	}

	// Initial shape — no `to_address`.
	row1 := timescale.SoroswapSkimEvent{
		ContractID:      pair,
		Ledger:          52_000_000,
		LedgerCloseTime: closedAt,
		TxHash:          txHash,
		OpIndex:         0,
		EventIndex:      0,
		To:              "",
		Amount0:         "7500",
		Amount1:         "1234567",
	}
	if err := store.InsertSoroswapSkimEvent(ctx, row1); err != nil {
		t.Fatalf("Insert row1: %v", err)
	}

	// Future-upgrade shape — body includes a `to` field.
	row2 := row1
	row2.LedgerCloseTime = closedAt.Add(time.Second)
	row2.Ledger = 52_000_001
	row2.EventIndex = 1
	row2.To = to
	if err := store.InsertSoroswapSkimEvent(ctx, row2); err != nil {
		t.Fatalf("Insert row2: %v", err)
	}

	// Above-int64 i128 — ADR-0003 boundary. amounts must round-trip
	// exactly through the NUMERIC column.
	row3 := row1
	row3.LedgerCloseTime = closedAt.Add(2 * time.Second)
	row3.Ledger = 52_000_002
	row3.EventIndex = 2
	row3.Amount0 = "123456789012345678901234567890" // > 2^96
	if err := store.InsertSoroswapSkimEvent(ctx, row3); err != nil {
		t.Fatalf("Insert row3 (big i128): %v", err)
	}

	// Idempotency: re-insert row1 — no-op via ON CONFLICT DO NOTHING.
	if err := store.InsertSoroswapSkimEvent(ctx, row1); err != nil {
		t.Fatalf("Re-insert row1: %v", err)
	}

	// Verify exactly 3 distinct rows landed, the amounts and
	// to_address columns survived the round-trip, and the
	// to_address NULL semantics work.
	var count int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM soroswap_skim_events WHERE contract_id = $1`, pair).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 3 {
		t.Errorf("row count = %d, want 3", count)
	}

	var nullCount, populatedCount int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM soroswap_skim_events WHERE to_address IS NULL`).Scan(&nullCount); err != nil {
		t.Fatalf("count null: %v", err)
	}
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM soroswap_skim_events WHERE to_address = $1`, to).Scan(&populatedCount); err != nil {
		t.Fatalf("count populated: %v", err)
	}
	if nullCount != 2 || populatedCount != 1 {
		t.Errorf("to_address split = (null=%d, populated=%d), want (2, 1)", nullCount, populatedCount)
	}

	var amt0Big sql.NullString
	if err := store.DB().QueryRowContext(ctx,
		`SELECT amount_0::text FROM soroswap_skim_events WHERE ledger = $1`, 52_000_002).
		Scan(&amt0Big); err != nil {
		t.Fatalf("scan big amount: %v", err)
	}
	if !amt0Big.Valid || amt0Big.String != "123456789012345678901234567890" {
		t.Errorf("big i128 round-trip lost precision: got %q want %q",
			amt0Big.String, "123456789012345678901234567890")
	}
}

// TestSoroswapSkimEvent_RejectsBadInputs verifies the defensive
// pre-flight checks before InsertSoroswapSkimEvent ever hits the DB.
// These are caller bugs, not chain truths — the decoder never emits
// the rejected shapes.
func TestSoroswapSkimEvent_RejectsBadInputs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	good := timescale.SoroswapSkimEvent{
		ContractID:      mustContractStrkey(t, 0xB0),
		Ledger:          1,
		LedgerCloseTime: time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		TxHash:          []byte("01234567890123456789012345678901"), // 32 bytes
		OpIndex:         0,
		EventIndex:      0,
		Amount0:         "1",
		Amount1:         "2",
	}

	bad := []struct {
		name string
		mut  func(*timescale.SoroswapSkimEvent)
	}{
		{"empty ContractID", func(e *timescale.SoroswapSkimEvent) { e.ContractID = "" }},
		{"empty TxHash", func(e *timescale.SoroswapSkimEvent) { e.TxHash = nil }},
		{"zero LedgerCloseTime", func(e *timescale.SoroswapSkimEvent) { e.LedgerCloseTime = time.Time{} }},
		{"empty Amount0", func(e *timescale.SoroswapSkimEvent) { e.Amount0 = "" }},
		{"empty Amount1", func(e *timescale.SoroswapSkimEvent) { e.Amount1 = "" }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			row := good
			tc.mut(&row)
			if err := store.InsertSoroswapSkimEvent(ctx, row); err == nil {
				t.Errorf("InsertSoroswapSkimEvent(%s) = nil, want error", tc.name)
			}
		})
	}
}

// mustContractStrkey mirrors the soroswap unit-test helper:
// builds a valid C-strkey from a single-byte seed for deterministic
// fixtures.
// TestSpectraEvents_Storage executes migration 0210 and the spectra store
// through real TimescaleDB: NUMERIC round-trip, idempotent re-insert,
// the market row merged from three discovery events in any order, the
// generation guard on both tables, and the CHECKs behind the Go validator.
func TestSpectraEvents_Storage(t *testing.T) {
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
		factory  = "CC4ZVRIYM33M5FVAUDFWK7JXO3PWIVSKKEBXVEEC5E6KPYISXIMLUJCP"
		registry = "CCUGRASBWD5SXDYMS7NM437FQ7KNKHFX74D2VRJVTRU4J2TWDMURUW3V"
		pt       = "CDRK5SWZ7DQJ4BZUAZQABPSP7MZS4NO4PPW7LW6CVD5THZDVE63MVLJJ"
		yt       = "CC3MKDR62O4Q7EEBOXXCNFJH3Z6AZQLB3QUTUEKAMSIJIYAHLUPVQR5G"
		ibt      = "CAHPZLEH6O6WJPICJAVRLCTYCAYDN52F4SM6IX3XJKWYSAASSHGZEZBO"
		holder   = "GCFB64LD5OX6XXUQW44LXAE7RAHORF2FJTSQ3DXEAV6DXVOML433X6HF"
		// 10^30: an 18-decimal market's amount well past 2^64 and 2^96.
		e30 = "1000000000000000000000000000000"
	)
	amt := func(s string) *canonical.Amount {
		a, err := canonical.FromString(s)
		if err != nil {
			t.Fatalf("amount %q: %v", s, err)
		}
		return &a
	}
	t0 := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	ev := func(tx string, idx uint32, kind timescale.SpectraEventKind, role timescale.SpectraRole, contract string) timescale.SpectraEvent {
		return timescale.SpectraEvent{
			ContractID: contract, Ledger: 64_400_000, LedgerCloseTime: t0,
			TxHash: strings.Repeat(tx, 64), EventIndex: idx, Kind: kind, Role: role,
		}
	}
	count := func(where string, args ...any) int {
		var n int
		if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM spectra_events WHERE `+where, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	t.Run("numeric round-trip and idempotent re-insert", func(t *testing.T) {
		mint := ev("a", 1, timescale.SpectraPTMinted, timescale.SpectraRolePT, pt)
		mint.MarketPT, mint.Caller, mint.Receiver, mint.Shares = pt, holder, holder, amt(e30)
		yield := ev("a", 2, timescale.SpectraYieldUpdated, timescale.SpectraRolePT, pt)
		yield.MarketPT, yield.Owner, yield.YieldInIBT = pt, holder, amt("-"+e30)
		transfer := ev("a", 3, timescale.SpectraTransfer, timescale.SpectraRoleYT, yt)
		transfer.MarketPT, transfer.Caller, transfer.Receiver, transfer.Amount = pt, holder, ibt, amt(e30+"7")

		for _, e := range []timescale.SpectraEvent{mint, yield, transfer, mint, transfer} {
			if err := store.InsertSpectraEvent(ctx, e); err != nil {
				t.Fatalf("insert %s: %v", e.Kind, err)
			}
		}
		if n := count(`tx_hash = $1`, mint.TxHash); n != 3 {
			t.Errorf("rows = %d, want 3 (re-insert must be idempotent)", n)
		}
		var shares, yieldStr, amount string
		if err := store.DB().QueryRowContext(ctx, `
			SELECT (SELECT shares::text FROM spectra_events WHERE tx_hash = $1 AND event_kind = 'pt_minted'),
			       (SELECT yield_in_ibt::text FROM spectra_events WHERE tx_hash = $1 AND event_kind = 'yield_updated'),
			       (SELECT amount::text FROM spectra_events WHERE tx_hash = $1 AND event_kind = 'transfer')`,
			mint.TxHash).Scan(&shares, &yieldStr, &amount); err != nil {
			t.Fatalf("read amounts: %v", err)
		}
		if shares != e30 || yieldStr != "-"+e30 || amount != e30+"7" {
			t.Errorf("shares/yield/amount = %s/%s/%s, want %s/-%s/%s7 — NUMERIC lost precision", shares, yieldStr, amount, e30, e30, e30)
		}
		var nulls int
		if err := store.DB().QueryRowContext(ctx, `
			SELECT num_nulls(owner, maker, order_id, ibt, yt, duration_s, vault_shares, assets, amount, yield_in_ibt)
			  FROM spectra_events WHERE tx_hash = $1 AND event_kind = 'pt_minted'`, mint.TxHash).Scan(&nulls); err != nil {
			t.Fatalf("read nulls: %v", err)
		}
		if nulls != 10 {
			t.Errorf("pt_minted has %d NULL columns of the 10 it does not carry, want 10", nulls)
		}
	})

	t.Run("market merged from discovery rows in any order", func(t *testing.T) {
		added := ev("b", 5, timescale.SpectraPTAdded, timescale.SpectraRoleRegistry, registry)
		added.MarketPT = pt
		ytDeployed := ev("b", 4, timescale.SpectraYTDeployed, timescale.SpectraRolePT, pt)
		ytDeployed.MarketPT, ytDeployed.YT = pt, yt
		deployed := ev("b", 3, timescale.SpectraPTDeployed, timescale.SpectraRoleFactory, factory)
		deployed.MarketPT, deployed.Caller, deployed.IBT, deployed.DurationSeconds = pt, holder, ibt, 2_592_000

		check := func(want timescale.SpectraMarket) {
			t.Helper()
			got, err := store.SpectraMarkets(ctx)
			if err != nil {
				t.Fatalf("SpectraMarkets: %v", err)
			}
			if len(got) != 1 || got[0] != want {
				t.Fatalf("markets = %+v, want [%+v]", got, want)
			}
		}

		if err := store.InsertSpectraEvent(ctx, added); err != nil {
			t.Fatalf("pt_added: %v", err)
		}
		check(timescale.SpectraMarket{PT: pt, ListedLedger: 64_400_000})

		if err := store.InsertSpectraEvent(ctx, ytDeployed); err != nil {
			t.Fatalf("yt_deployed: %v", err)
		}
		check(timescale.SpectraMarket{PT: pt, YT: yt, ListedLedger: 64_400_000})

		if err := store.InsertSpectraEvent(ctx, deployed); err != nil {
			t.Fatalf("pt_deployed: %v", err)
		}
		full := timescale.SpectraMarket{
			PT: pt, YT: yt, IBT: ibt, FactoryID: factory, Deployer: holder,
			DurationSeconds: 2_592_000, CreationLedger: 64_400_000, DeployedAt: t0,
			ListedLedger: 64_400_000,
		}
		check(full)

		// Replaying any one discovery row leaves the merged row intact.
		if err := store.InsertSpectraEvent(ctx, ytDeployed); err != nil {
			t.Fatalf("yt_deployed replay: %v", err)
		}
		check(full)
	})

	t.Run("generation guard covers the event and its market", func(t *testing.T) {
		t.Cleanup(func() { store.SetDeriveGeneration(0) })
		const pt2 = "CAAQJ6CN3KWJUG2CTUFUV27IL2BBWEIQKQA27HR7KFFZXROLHN6ELMBI"
		deployed := ev("c", 0, timescale.SpectraPTDeployed, timescale.SpectraRoleFactory, factory)
		deployed.MarketPT, deployed.Caller, deployed.IBT, deployed.DurationSeconds = pt2, holder, ibt, 100
		read := func() (eventDuration, marketDuration, gen int64) {
			t.Helper()
			if err := store.DB().QueryRowContext(ctx, `
				SELECT e.duration_s, m.duration_s, e.derive_generation
				  FROM spectra_events e JOIN spectra_markets m ON m.pt = e.market_pt
				 WHERE e.tx_hash = $1`, deployed.TxHash).Scan(&eventDuration, &marketDuration, &gen); err != nil {
				t.Fatalf("read: %v", err)
			}
			return
		}

		if err := store.InsertSpectraEvent(ctx, deployed); err != nil {
			t.Fatalf("gen0 insert: %v", err)
		}
		store.SetDeriveGeneration(1)
		corrected := deployed
		corrected.DurationSeconds = 2_592_000
		if err := store.InsertSpectraEvent(ctx, corrected); err != nil {
			t.Fatalf("gen1 insert: %v", err)
		}
		if e, m, g := read(); e != 2_592_000 || m != 2_592_000 || g != 1 {
			t.Fatalf("after gen1: event/market duration, gen = %d/%d, %d; want 2592000/2592000, 1", e, m, g)
		}
		store.SetDeriveGeneration(0)
		if err := store.InsertSpectraEvent(ctx, deployed); err != nil {
			t.Fatalf("gen0 replay: %v", err)
		}
		if e, m, g := read(); e != 2_592_000 || m != 2_592_000 || g != 1 {
			t.Errorf("after gen0 replay: event/market duration, gen = %d/%d, %d; want 2592000/2592000, 1 — a stale replay reverted the correction", e, m, g)
		}
	})

	t.Run("a rebuild that re-keys pt_deployed drops the stale market", func(t *testing.T) {
		t.Cleanup(func() { store.SetDeriveGeneration(0) })
		const (
			stale = "CBZ7M5B3Y4WWBZ5XK74ZKMGNC3NKUGBVLBWJ5CZJKEXAEDW3Q4HYXC2A"
			fresh = "CDPGNJ6ZMPGIJOHJH2LTZWC2ZOSVVQJ6WN7HPTPDE5EPZT3BXCFRCYUK"
		)
		deployed := ev("e", 0, timescale.SpectraPTDeployed, timescale.SpectraRoleFactory, factory)
		deployed.MarketPT, deployed.Caller, deployed.IBT, deployed.DurationSeconds = stale, holder, ibt, 100
		if err := store.InsertSpectraEvent(ctx, deployed); err != nil {
			t.Fatalf("gen0 insert: %v", err)
		}
		store.SetDeriveGeneration(1)
		corrected := deployed
		corrected.MarketPT = fresh
		if err := store.InsertSpectraEvent(ctx, corrected); err != nil {
			t.Fatalf("gen1 insert: %v", err)
		}
		markets, err := store.SpectraMarkets(ctx)
		if err != nil {
			t.Fatalf("SpectraMarkets: %v", err)
		}
		var sawFresh bool
		for _, m := range markets {
			if m.PT == stale {
				t.Errorf("market %s survives the rebuild that re-keyed its only discovery row", stale)
			}
			sawFresh = sawFresh || m.PT == fresh
		}
		if !sawFresh {
			t.Errorf("market %s missing after the rebuild", fresh)
		}
	})

	t.Run("CHECKs refuse what the validator refuses", func(t *testing.T) {
		const ins = `
			INSERT INTO spectra_events (ledger_close_time, contract_id, ledger, tx_hash, op_index,
			    event_index, event_kind, role, market_pt, caller, receiver, amount, shares)
			VALUES ($1, $2, 1, $3, 0, $4, $5, $6, $7, $8, $9, $10, $11)`
		tx := strings.Repeat("d", 64)
		cases := []struct {
			name string
			args []any
		}{
			{"transfer with an extra shares column", []any{0, "transfer", "pt", pt, holder, ibt, "1", "1"}},
			{"pt_minted from an IBT", []any{1, "pt_minted", "ibt", pt, holder, holder, nil, "1"}},
			{"PT event naming another market", []any{2, "transfer", "pt", yt, holder, ibt, "1", nil}},
			{"negative transfer amount", []any{3, "transfer", "yt", pt, holder, ibt, "-1", nil}},
		}
		for _, c := range cases {
			args := append([]any{t0, pt, tx}, c.args...)
			if _, err := store.DB().ExecContext(ctx, ins, args...); err == nil {
				t.Errorf("%s: accepted; CHECK missing", c.name)
			}
		}
		if _, err := store.DB().ExecContext(ctx, `INSERT INTO spectra_markets (pt, ibt) VALUES ('X', 'Y')`); err == nil {
			t.Error("spectra_markets accepted ibt without the other pt_deployed columns")
		}
	})
}

// TestSushiswapV3Pools_UpsertLoadRoundTrip executes migration 0202 and proves
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

	pool := mustContractStrkey(t, 0xD0)
	factory := mustContractStrkey(t, 0xD1)
	tok0 := mustContractStrkey(t, 0xD2)
	tok1 := mustContractStrkey(t, 0xD3)

	want := timescale.SushiswapV3Pool{
		PoolID: pool, FactoryID: factory, Token0: tok0, Token1: tok1,
		FeePips: 500, TickSpacing: 10, CreationLedger: 61_487_379,
	}
	if err := store.UpsertSushiswapV3Pool(ctx, want); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := store.LoadSushiswapV3Pools(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Load = %#v, want [%#v]", got, want)
	}

	// A conflicting upsert with changed columns must overwrite in place.
	changed := want
	changed.FeePips = 3000
	changed.TickSpacing = 60
	changed.Token1 = mustContractStrkey(t, 0xD4)
	if err := store.UpsertSushiswapV3Pool(ctx, changed); err != nil {
		t.Fatalf("re-Upsert changed: %v", err)
	}
	got, err = store.LoadSushiswapV3Pools(ctx)
	if err != nil {
		t.Fatalf("Load after re-upsert: %v", err)
	}
	if len(got) != 1 || got[0] != changed {
		t.Fatalf("Load after re-upsert = %#v, want [%#v]", got, changed)
	}
}

// TestSushiswapV3PositionEvents_RoundTrip executes the 0203 schema through
// real TimescaleDB: one row per action, a collect with NULL liquidity, the
// CHECK that refuses a mint without liquidity, u128 amounts above 2^64
// surviving NUMERIC, and the idempotent re-insert.
func TestSushiswapV3PositionEvents_RoundTrip(t *testing.T) {
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
		pool   = "CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3"
		owner  = "CARTUL5AWDZYBSN7HUUJZSKCAKCIAKM7M54Z76G6KRYCK4XPR3OHUQZ4"
		sender = "GCFB64LD5OX6XXUQW44LXAE7RAHORF2FJTSQ3DXEAV6DXVOML433X6HF"
		token0 = "native"
		token1 = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
		// 2^128 - 1: the widest u128 a pool event can carry.
		maxU128 = "340282366920938463463374607431768211455"
	)
	txHash, _ := hex.DecodeString(strings.Repeat("ab", 32))
	t0 := time.Date(2026, 3, 3, 20, 52, 22, 0, time.UTC)

	base := timescale.SushiswapV3PositionEvent{
		Pool: pool, Ledger: 61_487_383, LedgerCloseTime: t0, TxHash: txHash,
		OpIndex: 0, EventIndex: 3, Owner: owner,
		Token0: token0, Token1: token1,
		TickLower: -18780, TickUpper: -15180,
		Amount0: "100000000", Amount1: "18231333",
	}

	mint := base
	mint.Action, mint.Sender, mint.Liquidity = "mint", sender, "496117982"
	burn := base
	burn.EventIndex, burn.Action, burn.Liquidity = 4, "burn", maxU128
	burn.Amount0 = maxU128
	collect := base
	collect.EventIndex, collect.Action = 5, "collect"

	for _, e := range []timescale.SushiswapV3PositionEvent{mint, burn, collect, mint} {
		if err := store.InsertSushiswapV3PositionEvent(ctx, e); err != nil {
			t.Fatalf("insert %s: %v", e.Action, err)
		}
	}

	var n int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM sushiswap_v3_position_events WHERE pool = $1`, pool).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("rows = %d, want 3 (mint re-insert must be idempotent)", n)
	}

	var amount0, liq string
	if err := store.DB().QueryRowContext(ctx, `
		SELECT amount_0::text, liquidity::text FROM sushiswap_v3_position_events
		WHERE pool = $1 AND action = 'burn'`, pool).Scan(&amount0, &liq); err != nil {
		t.Fatalf("read burn: %v", err)
	}
	if amount0 != maxU128 || liq != maxU128 {
		t.Errorf("burn amount0/liquidity = %s/%s, want %s — u128 lost precision", amount0, liq, maxU128)
	}

	var nullLiq, senderNull bool
	if err := store.DB().QueryRowContext(ctx, `
		SELECT liquidity IS NULL, sender IS NULL FROM sushiswap_v3_position_events
		WHERE pool = $1 AND action = 'collect'`, pool).Scan(&nullLiq, &senderNull); err != nil {
		t.Fatalf("read collect: %v", err)
	}
	if !nullLiq || !senderNull {
		t.Errorf("collect liquidity NULL = %v, sender NULL = %v, want both true", nullLiq, senderNull)
	}

	// The CHECK is the backstop behind the Go validator: raw SQL must be
	// refused too.
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sushiswap_v3_position_events (
			ledger_close_time, pool, ledger, tx_hash, op_index, event_index,
			action, owner, token_0, token_1, tick_lower, tick_upper,
			liquidity, amount_0, amount_1)
		VALUES ($1, $2, 1, $3, 0, 9, 'mint', $4, $5, $6, 0, 1, NULL, 1, 1)`,
		t0, pool, txHash, owner, token0, token1)
	if err == nil {
		t.Error("mint with NULL liquidity was accepted; CHECK constraint missing")
	}
}

// TestSushiswapV3PositionEvents_GenerationGuard: a corrected gen-1 re-derive
// replaces the live gen-0 row, and a later gen-0 replay of the same event
// cannot revert it.
func TestSushiswapV3PositionEvents_GenerationGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	txHash, _ := hex.DecodeString(strings.Repeat("cd", 32))
	e := timescale.SushiswapV3PositionEvent{
		Pool:   "CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3",
		Ledger: 61_487_400, LedgerCloseTime: time.Date(2026, 3, 3, 21, 0, 0, 0, time.UTC),
		TxHash: txHash, EventIndex: 1, Action: "mint", Liquidity: "10",
		Owner:  "CARTUL5AWDZYBSN7HUUJZSKCAKCIAKM7M54Z76G6KRYCK4XPR3OHUQZ4",
		Sender: "GCFB64LD5OX6XXUQW44LXAE7RAHORF2FJTSQ3DXEAV6DXVOML433X6HF",
		Token0: "native", Token1: "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		TickLower: -10, TickUpper: 10, Amount0: "111", Amount1: "222",
	}
	read := func() (string, int64) {
		var a string
		var g int64
		if err := store.DB().QueryRowContext(ctx, `
			SELECT amount_0::text, derive_generation FROM sushiswap_v3_position_events
			WHERE pool = $1 AND ledger = $2`, e.Pool, e.Ledger).Scan(&a, &g); err != nil {
			t.Fatalf("read: %v", err)
		}
		return a, g
	}

	if err := store.InsertSushiswapV3PositionEvent(ctx, e); err != nil {
		t.Fatalf("gen0 insert: %v", err)
	}

	store.SetDeriveGeneration(1)
	corrected := e
	corrected.Amount0 = "999"
	if err := store.InsertSushiswapV3PositionEvent(ctx, corrected); err != nil {
		t.Fatalf("gen1 insert: %v", err)
	}
	if a, g := read(); a != "999" || g != 1 {
		t.Fatalf("after gen1 write amount_0/gen = %s/%d, want 999/1", a, g)
	}

	store.SetDeriveGeneration(0)
	if err := store.InsertSushiswapV3PositionEvent(ctx, e); err != nil {
		t.Fatalf("gen0 replay: %v", err)
	}
	if a, g := read(); a != "999" || g != 1 {
		t.Errorf("after gen0 replay amount_0/gen = %s/%d, want 999/1 — gen0 reverted the corrected row", a, g)
	}
}
