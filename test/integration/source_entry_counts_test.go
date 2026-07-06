//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	c "github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
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
