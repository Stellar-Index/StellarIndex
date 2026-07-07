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
// CS-026 un-gated-source caveat (comet matches on topic bytes alone).
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
		t.Errorf("comet block missing the CS-026 un-gated caveat note; Notes=%+v", blk.Notes)
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
	if sk.Amount0.BigInt().Cmp(wantAmt0) != 0 {
		t.Errorf("Amount0 = %s, want %s — i128/NUMERIC lost precision", sk.Amount0, wantAmt0)
	}
	if sk.Pairs != 1 {
		t.Errorf("Pairs = %d, want 1", sk.Pairs)
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
	assertKPI(t, kpis, "Skimmed token0 (90d)", wantAmt0.String())
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
