//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSourceEntryCounts_AtomicIdempotentBump is the correctness core
// of the always-on entry tally (migration 0035): the writers bump
// source_entry_counts ATOMICALLY and IDEMPOTENTLY. A backfill
// re-walk that re-inserts already-stored rows (ON CONFLICT DO
// NOTHING → 0 rows) must NOT inflate the tally — otherwise every
// `-resume` / parallel-chunk replay would drift the count upward,
// re-creating exactly the "legacy data" class of bug this design
// exists to avoid.
func TestSourceEntryCounts_AtomicIdempotentBump(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlm, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSD, _ := c.NewPair(xlm, usd)
	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	count := func(source string) int64 {
		m, err := store.SourceEntryCounts(ctx)
		if err != nil {
			t.Fatalf("SourceEntryCounts: %v", err)
		}
		return m[source]
	}

	tr1 := mkIntegrationTrade("sdex", 1, ts, xlmUSD, 100_000_000, 12_000_000)
	tr2 := mkIntegrationTrade("sdex", 2, ts, xlmUSD, 100_000_000, 12_000_000)

	// First insert → tally 1.
	if err := store.InsertTrade(ctx, tr1); err != nil {
		t.Fatalf("InsertTrade tr1: %v", err)
	}
	if got := count("sdex"); got != 1 {
		t.Fatalf("after first insert: sdex entries = %d, want 1", got)
	}

	// Re-insert the SAME trade (backfill re-walk). PK conflict →
	// DO NOTHING → the HAVING-gated counter upsert must be a no-op.
	if err := store.InsertTrade(ctx, tr1); err != nil {
		t.Fatalf("InsertTrade tr1 (replay): %v", err)
	}
	if got := count("sdex"); got != 1 {
		t.Fatalf("after replay: sdex entries = %d, want 1 (idempotent)", got)
	}

	// A genuinely new trade for the same source → tally 2.
	if err := store.InsertTrade(ctx, tr2); err != nil {
		t.Fatalf("InsertTrade tr2: %v", err)
	}
	if got := count("sdex"); got != 2 {
		t.Fatalf("after second distinct insert: sdex entries = %d, want 2", got)
	}

	// Oracle updates feed the SAME tally (the whole point of the
	// rename: "entries", not "trades"). Same idempotency contract.
	ou := c.OracleUpdate{
		Source:    "reflector-dex",
		Ledger:    50_000_123,
		TxHash:    strings.Repeat("ab", 32),
		OpIndex:   0,
		Timestamp: ts,
		Asset:     xlm,
		Quote:     usd,
		Price:     c.NewAmount(big.NewInt(1_2345678901234)),
		Decimals:  14,
	}
	if err := store.InsertOracleUpdate(ctx, ou); err != nil {
		t.Fatalf("InsertOracleUpdate: %v", err)
	}
	if err := store.InsertOracleUpdate(ctx, ou); err != nil {
		t.Fatalf("InsertOracleUpdate (replay): %v", err)
	}
	if got := count("reflector-dex"); got != 1 {
		t.Fatalf("oracle entries = %d, want 1 (idempotent across oracle_updates)", got)
	}
	// Trade tally untouched by oracle ingest.
	if got := count("sdex"); got != 2 {
		t.Fatalf("sdex entries drifted to %d after oracle insert, want 2", got)
	}

	// SeedSourceEntryCounts is the authoritative reconcile: it must
	// CORRECT drift (SET, not ADD). Poison the tally, reseed, verify
	// it snaps back to the real table totals.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE source_entry_counts SET entry_count = 99999 WHERE source = 'sdex'`); err != nil {
		t.Fatalf("poison: %v", err)
	}
	if _, err := store.SeedSourceEntryCounts(ctx); err != nil {
		t.Fatalf("SeedSourceEntryCounts: %v", err)
	}
	if got := count("sdex"); got != 2 {
		t.Fatalf("after reseed: sdex entries = %d, want 2 (authoritative recount)", got)
	}
	if got := count("reflector-dex"); got != 1 {
		t.Fatalf("after reseed: reflector-dex entries = %d, want 1", got)
	}
}

// TestSourceEntryCounts_LogOnlySourcesReconcile closes the reconcile gap
// for the "log-only" sinks (soroswap-router, defindex): the sink bumps
// source_entry_counts by 1 per decoded event (NON-idempotently), so a
// replay / re-derive that re-drives the sink double-counts them
// permanently — a KALE-class trap for the `entries` diagnostics column.
//
// Both sinks now ALSO persist one idempotent row per event to a countable
// hypertable (soroswap_router_swaps / defindex_flows). This test proves
// SeedSourceEntryCounts folds those tables in and SET-resets the two
// sources authoritatively — so the drift a replay introduces is CORRECTED
// (previously the doc warned operators must NOT seed-reset them because
// there was "no table to recompute from").
func TestSourceEntryCounts_LogOnlySourcesReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	count := func(source string) int64 {
		m, err := store.SourceEntryCounts(ctx)
		if err != nil {
			t.Fatalf("SourceEntryCounts: %v", err)
		}
		return m[source]
	}
	ts := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	txh := func(n int) string { return fmt.Sprintf("%064x", n) }

	// insertRouter mimics the sink: persist one idempotent row + bump the
	// counter (the non-idempotent per-event bump).
	insertRouter := func(nonce int) {
		t.Helper()
		row := timescale.SoroswapRouterSwap{
			Ledger:          uint32(1000 + nonce),
			LedgerCloseTime: ts.Add(time.Duration(nonce) * time.Second),
			TxHash:          txh(nonce),
			OpIndex:         0,
			ContractID:      "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6",
			FunctionName:    "swap_exact_tokens_for_tokens",
			Recipient:       "GA1IF6WRUM4NRJIF7SDBEK4HXQFLA33MB47AR33YHV5EDJKC742OCLEV",
			Path:            []string{"native", "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"},
			AmountIn:        "1000",
			AmountOut:       "990",
			CallSig:         txh(nonce), // per-call PK discriminator
		}
		if err := store.InsertSoroswapRouterSwap(ctx, row); err != nil {
			t.Fatalf("InsertSoroswapRouterSwap[%d]: %v", nonce, err)
		}
		if err := store.BumpSourceEntryCount(ctx, "soroswap-router", 1); err != nil {
			t.Fatalf("bump router[%d]: %v", nonce, err)
		}
	}
	// insertDefindex mimics the sink for one flow (strategy or vault layer).
	insertDefindex := func(nonce int, layer timescale.DefindexLayer) {
		t.Helper()
		row := timescale.DefindexFlow{
			Ledger:          uint32(2000 + nonce),
			LedgerCloseTime: ts.Add(time.Duration(nonce) * time.Second),
			TxHash:          txh(1000 + nonce),
			OpIndex:         0,
			EventIndex:      0,
			ContractID:      "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
			Layer:           layer,
			Direction:       timescale.DefindexDeposit,
			Actor:           "GA1IF6WRUM4NRJIF7SDBEK4HXQFLA33MB47AR33YHV5EDJKC742OCLEV",
		}
		switch layer {
		case timescale.DefindexLayerStrategy:
			row.Amount = "5000"
		case timescale.DefindexLayerVault:
			row.AmountsVec = []string{"5000"}
			row.DfTokens = "4998"
		}
		if err := store.InsertDefindexFlow(ctx, row); err != nil {
			t.Fatalf("InsertDefindexFlow[%d]: %v", nonce, err)
		}
		if err := store.BumpSourceEntryCount(ctx, "defindex", 1); err != nil {
			t.Fatalf("bump defindex[%d]: %v", nonce, err)
		}
	}

	// ─── Steady-state ingest: 3 router swaps, 2 defindex flows ───────────
	for i := 1; i <= 3; i++ {
		insertRouter(i)
	}
	insertDefindex(1, timescale.DefindexLayerStrategy)
	insertDefindex(2, timescale.DefindexLayerVault)
	if got := count("soroswap-router"); got != 3 {
		t.Fatalf("router entries = %d, want 3", got)
	}
	if got := count("defindex"); got != 2 {
		t.Fatalf("defindex entries = %d, want 2", got)
	}

	// ─── Replay the SAME range: table inserts DO NOTHING (idempotent),
	//     but the per-event bump still ADDs → the counter double-counts.
	for i := 1; i <= 3; i++ {
		insertRouter(i)
	}
	insertDefindex(1, timescale.DefindexLayerStrategy)
	insertDefindex(2, timescale.DefindexLayerVault)
	if got := count("soroswap-router"); got != 6 {
		t.Fatalf("router entries after replay = %d, want 6 (bump is not replay-safe — the drift this fix reconciles)", got)
	}
	if got := count("defindex"); got != 4 {
		t.Fatalf("defindex entries after replay = %d, want 4 (bump double-counted)", got)
	}

	// ─── Authoritative reconcile: SET-reset from the countable tables ────
	if _, err := store.SeedSourceEntryCounts(ctx); err != nil {
		t.Fatalf("SeedSourceEntryCounts: %v", err)
	}
	if got := count("soroswap-router"); got != 3 {
		t.Fatalf("router entries after reseed = %d, want 3 (snapped back to soroswap_router_swaps COUNT)", got)
	}
	if got := count("defindex"); got != 2 {
		t.Fatalf("defindex entries after reseed = %d, want 2 (snapped back to defindex_flows COUNT — both layers)", got)
	}
}

// TestSourceEntryCounts_GappedNonTradeSinksReconcile extends the
// log-only reconcile guarantee to EVERY remaining source that bumps
// source_entry_counts through pipeline/sink.go::bumpEntryCount (a
// non-idempotent +1 per decoded event): comet liquidity, soroswap
// skim, phoenix liquidity/stake, blend positions/emissions/admin,
// blend-backstop, cctp, rozo, sep41_transfers.
//
// Each writes one idempotent (ON CONFLICT DO NOTHING) row per event to
// a countable hypertable, so its COUNT is replay-stable and equals the
// bump total. This test drives each sink twice over the SAME range —
// the second pass is a replay: the table INSERT is a no-op but the bump
// ADDs, double-counting — then proves SeedSourceEntryCounts folds every
// table in and SET-resets each source back to the honest total.
//
// comet / soroswap / phoenix ALSO write a swap to `trades` (bumped
// idempotently INSIDE InsertTrade, so replay-safe). Their steady-state
// entries therefore = #swaps + #non-swap-events; the test asserts the
// seed sums the trades count with the folded non-trade table WITHOUT
// double-counting (disjoint event sets).
func TestSourceEntryCounts_GappedNonTradeSinksReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	count := func(source string) int64 {
		m, err := store.SourceEntryCounts(ctx)
		if err != nil {
			t.Fatalf("SourceEntryCounts: %v", err)
		}
		return m[source]
	}
	bump := func(source string) {
		t.Helper()
		if err := store.BumpSourceEntryCount(ctx, source, 1); err != nil {
			t.Fatalf("bump %s: %v", source, err)
		}
	}

	xlm, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSD, _ := c.NewPair(xlm, usd)
	ts := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)

	const (
		contractID = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
		poolID     = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		userAddr   = "GA1IF6WRUM4NRJIF7SDBEK4HXQFLA33MB47AR33YHV5EDJKC742OCLEV"
	)

	// Each closure below inserts a FIXED row — calling it twice is a
	// replay (same PK → ON CONFLICT DO NOTHING). Non-trade closures also
	// call bump(...) to mirror the sink's NON-idempotent bumpEntryCount.
	// The trade closures rely on InsertTrade's idempotent internal bump.

	insertCometSwap := func() {
		t.Helper()
		if err := store.InsertTrade(ctx, mkIntegrationTrade("comet", 1, ts, xlmUSD, 100_000_000, 12_000_000)); err != nil {
			t.Fatalf("InsertTrade comet: %v", err)
		}
	}
	insertCometLiquidity := func() {
		t.Helper()
		if err := store.InsertCometLiquidity(ctx, timescale.CometLiquidityEvent{
			ContractID: contractID, Ledger: 1001, LedgerCloseTime: ts,
			TxHash: strings.Repeat("a1", 32), OpIndex: 0, EventIndex: 0,
			Kind: timescale.CometLiquidityJoinPool, Caller: userAddr, Token: contractID,
			Amount: c.NewAmount(big.NewInt(5_000)), PoolAmountIn: c.NewAmount(big.NewInt(0)),
		}); err != nil {
			t.Fatalf("InsertCometLiquidity: %v", err)
		}
		bump("comet")
	}

	insertSoroswapSwap := func() {
		t.Helper()
		if err := store.InsertTrade(ctx, mkIntegrationTrade("soroswap", 1, ts, xlmUSD, 100_000_000, 12_000_000)); err != nil {
			t.Fatalf("InsertTrade soroswap: %v", err)
		}
	}
	insertSoroswapSkim := func() {
		t.Helper()
		if err := store.InsertSoroswapSkimEvent(ctx, timescale.SoroswapSkimEvent{
			ContractID: contractID, Ledger: 1002, LedgerCloseTime: ts,
			TxHash: []byte(strings.Repeat("s", 32)), OpIndex: 0, EventIndex: 0,
			To: userAddr, Amount0: "1000", Amount1: "990",
		}); err != nil {
			t.Fatalf("InsertSoroswapSkimEvent: %v", err)
		}
		bump("soroswap")
	}

	insertPhoenixSwap := func() {
		t.Helper()
		if err := store.InsertTrade(ctx, mkIntegrationTrade("phoenix", 1, ts, xlmUSD, 100_000_000, 12_000_000)); err != nil {
			t.Fatalf("InsertTrade phoenix: %v", err)
		}
	}
	insertPhoenixLiquidity := func() {
		t.Helper()
		if err := store.InsertPhoenixLiquidityChange(ctx, timescale.PhoenixLiquidityChange{
			Pool: poolID, Ledger: 1003, ObservedAt: ts, TxHash: strings.Repeat("b2", 32),
			OpIndex: 0, EventIndex: 0, Action: timescale.PhoenixProvideLiquidity,
			Sender: userAddr, TokenA: contractID, TokenB: poolID, AmountA: "1000", AmountB: "2000",
		}); err != nil {
			t.Fatalf("InsertPhoenixLiquidityChange: %v", err)
		}
		bump("phoenix")
	}
	insertPhoenixStake := func() {
		t.Helper()
		if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
			StakeContract: contractID, Ledger: 1004, ObservedAt: ts, TxHash: strings.Repeat("c3", 32),
			OpIndex: 0, EventIndex: 0, Action: timescale.PhoenixBond, User: userAddr,
			LPToken: poolID, Amount: "1000",
		}); err != nil {
			t.Fatalf("InsertPhoenixStakeEvent: %v", err)
		}
		bump("phoenix")
	}

	insertBlendPosition := func() {
		t.Helper()
		if err := store.InsertBlendPositionEvent(ctx, domain.BlendPositionEvent{
			Pool: poolID, Kind: blend.EventSupply, Asset: contractID, User: userAddr,
			TokenAmount: big.NewInt(1_000_000), BOrDAmount: big.NewInt(990_000),
			Ledger: 2001, TxHash: pad64("e", 1), OpIndex: 0, Timestamp: ts,
		}); err != nil {
			t.Fatalf("InsertBlendPositionEvent: %v", err)
		}
		bump("blend")
	}
	insertBlendEmission := func() {
		t.Helper()
		if err := store.InsertBlendEmissionEvent(ctx, domain.BlendEmissionEvent{
			Pool: poolID, Kind: blend.EventGulp, Asset: contractID, Amount: big.NewInt(100),
			Ledger: 2002, TxHash: pad64("f", 2), OpIndex: 0, Timestamp: ts,
		}); err != nil {
			t.Fatalf("InsertBlendEmissionEvent: %v", err)
		}
		bump("blend")
	}
	insertBlendAdmin := func() {
		t.Helper()
		if err := store.InsertBlendAdminEvent(ctx, domain.BlendAdminEvent{
			ContractID: poolID, Kind: blend.EventSetAdmin, Admin: userAddr, Target: userAddr,
			Ledger: 2003, TxHash: pad64("a", 3), OpIndex: 0, Timestamp: ts,
		}); err != nil {
			t.Fatalf("InsertBlendAdminEvent: %v", err)
		}
		bump("blend")
	}

	insertBackstop := func() {
		t.Helper()
		if err := store.InsertBlendBackstopEvent(ctx, timescale.BlendBackstopEvent{
			ContractID: contractID, Ledger: 3001, TxHash: strings.Repeat("d4", 32),
			OpIndex: 0, EventIndex: 0, ObservedAt: ts, EventType: timescale.BackstopDeposit,
			Pool: poolID, UserAddress: userAddr, Amount: "1000",
		}); err != nil {
			t.Fatalf("InsertBlendBackstopEvent: %v", err)
		}
		bump("blend_backstop")
	}
	insertCCTP := func() {
		t.Helper()
		if err := store.InsertCCTPEvent(ctx, timescale.CCTPEvent{
			ContractID: contractID, Ledger: 3002, TxHash: strings.Repeat("e5", 32),
			OpIndex: 0, ObservedAt: ts, EventType: timescale.CCTPDepositForBurn, Amount: "1000",
		}); err != nil {
			t.Fatalf("InsertCCTPEvent: %v", err)
		}
		bump("cctp")
	}
	insertRozo := func() {
		t.Helper()
		if err := store.InsertRozoEvent(ctx, timescale.RozoEvent{
			ContractID: contractID, Ledger: 3003, TxHash: strings.Repeat("f6", 32),
			OpIndex: 0, ObservedAt: ts, EventType: timescale.RozoPayment,
			Amount: "1000", Destination: userAddr,
		}); err != nil {
			t.Fatalf("InsertRozoEvent: %v", err)
		}
		bump("rozo")
	}
	insertSEP41Transfer := func() {
		t.Helper()
		if err := store.InsertSEP41Transfer(ctx, timescale.SEP41TransferRow{
			ContractID: contractID, Ledger: 3004, TxHash: strings.Repeat("07", 32),
			OpIndex: 0, EventIndex: 0, ObservedAt: ts, Kind: timescale.SEP41Transfer,
			FromAddr: userAddr, ToAddr: userAddr, Amount: big.NewInt(1000),
		}); err != nil {
			t.Fatalf("InsertSEP41Transfer: %v", err)
		}
		bump("sep41_transfers")
	}

	drive := func() {
		insertCometSwap()
		insertCometLiquidity()
		insertSoroswapSwap()
		insertSoroswapSkim()
		insertPhoenixSwap()
		insertPhoenixLiquidity()
		insertPhoenixStake()
		insertBlendPosition()
		insertBlendEmission()
		insertBlendAdmin()
		insertBackstop()
		insertCCTP()
		insertRozo()
		insertSEP41Transfer()
	}

	// steady = the correct entry total once the fold is in place:
	//   comet   = 1 swap + 1 liquidity          = 2
	//   soroswap= 1 swap + 1 skim               = 2
	//   phoenix = 1 swap + 1 liquidity + 1 stake= 3
	//   blend   = 1 position + 1 emission + 1 admin = 3
	//   others  = 1 each
	steady := map[string]int64{
		"comet": 2, "soroswap": 2, "phoenix": 3, "blend": 3,
		"blend_backstop": 1, "cctp": 1, "rozo": 1, "sep41_transfers": 1,
	}

	// ─── Steady-state ingest ─────────────────────────────────────────
	drive()
	for src, want := range steady {
		if got := count(src); got != want {
			t.Fatalf("steady: %s entries = %d, want %d", src, got, want)
		}
	}

	// ─── Replay the SAME range: idempotent rows DO NOTHING, but the
	//     per-event bumpEntryCount still ADDs. Trade swaps are bumped
	//     inside InsertTrade (HAVING-gated) so they DON'T double; only
	//     the bumpEntryCount portion drifts.
	drive()
	drifted := map[string]int64{
		"comet": 3, "soroswap": 3, "phoenix": 5, "blend": 6,
		"blend_backstop": 2, "cctp": 2, "rozo": 2, "sep41_transfers": 2,
	}
	for src, want := range drifted {
		if got := count(src); got != want {
			t.Fatalf("after replay: %s entries = %d, want %d (bumpEntryCount is not replay-safe — the drift this fix reconciles)", src, got, want)
		}
	}

	// ─── Authoritative reconcile: SET-reset from the folded tables ───
	if _, err := store.SeedSourceEntryCounts(ctx); err != nil {
		t.Fatalf("SeedSourceEntryCounts: %v", err)
	}
	for src, want := range steady {
		if got := count(src); got != want {
			t.Fatalf("after reseed: %s entries = %d, want %d (should snap back to the folded table COUNT — for comet/soroswap/phoenix, trades + non-trade table, no double-count)", src, got, want)
		}
	}
}
