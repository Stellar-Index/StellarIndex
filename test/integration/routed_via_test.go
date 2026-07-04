//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	c "github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// TestRoutedViaTaggingAndRollup covers migration 0025 Phase B
// end-to-end against a real TimescaleDB:
//
//  1. TagTradesRoutedVia joins soroswap_router_swaps → trades on
//     (ledger, tx_hash), scoped to source='soroswap' — a same-tx
//     trade from another protocol is never tagged.
//  2. First-wins: an existing routed_via (from a different router)
//     is never overwritten.
//  3. Idempotence: re-running the same window tags zero rows.
//  4. AggregatorRollup math: per-router trade counts honour the
//     `since` bound, LastRoutedAt = max(ts), volume is NULL (not
//     zero) when no routed trade carries usd_volume, and the
//     registry seeds (0032/0033) surface with the 0072-aligned
//     'soroswap-router' name.
func TestRoutedViaTaggingAndRollup(t *testing.T) { //nolint:gocognit // linear scenario walk
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(c.NativeAsset(), usdc)

	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	t1 := t0.Add(5 * time.Minute)

	// Tx A (ledger 100): a 3-hop router swap → two soroswap leg
	// trades + one same-tx phoenix trade (must stay untagged).
	txA := mkIntegrationTrade("soroswap", 1, t0, pair, 1_000_000_000, 12_000_000)
	txA.Ledger = 100
	txA2 := txA
	txA2.OpIndex = txA.OpIndex + 1
	txAPhoenix := mkIntegrationTrade("phoenix", 1, t0, pair, 500_000_000, 6_000_000)
	txAPhoenix.Ledger = 100
	txAPhoenix.TxHash = txA.TxHash // same tx!

	// Tx B (ledger 101): one soroswap trade, pre-tagged by a
	// DIFFERENT router (first-wins fixture).
	txB := mkIntegrationTrade("soroswap", 2, t1, pair, 1_000_000_000, 12_100_000)
	txB.Ledger = 101

	// Unrelated soroswap trade (ledger 102, no router call).
	direct := mkIntegrationTrade("soroswap", 3, t1.Add(time.Minute), pair, 1_000_000_000, 12_200_000)
	direct.Ledger = 102

	for _, tr := range []c.Trade{txA, txA2, txAPhoenix, txB, direct} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	// Router invocations for tx A + tx B.
	for i, swap := range []struct {
		ledger uint32
		ts     time.Time
		txHash string
	}{
		{100, t0, txA.TxHash},
		{101, t1, txB.TxHash},
	} {
		row := timescale.SoroswapRouterSwap{
			Ledger:          swap.ledger,
			LedgerCloseTime: swap.ts,
			TxHash:          swap.txHash,
			OpIndex:         0,
			ContractID:      "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH",
			FunctionName:    "swap_exact_tokens_for_tokens",
			Recipient:       "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			Path: []string{
				"CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
				"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
			},
			AmountIn:  "1000000000",
			AmountOut: "12000000",
			CallSig:   "cafebabecafebabecafebabecafebab" + string(rune('0'+i)),
		}
		if err := store.InsertSoroswapRouterSwap(ctx, row); err != nil {
			t.Fatalf("InsertSoroswapRouterSwap: %v", err)
		}
	}

	windowFrom, windowTo := t0.Add(-time.Hour), t1.Add(time.Hour)

	// First-wins fixture: tag tx B's window as a different router
	// BEFORE the real pass runs.
	if n, err := store.TagTradesRoutedVia(ctx, "other-router", "soroswap", t1.Add(-time.Second), t1.Add(time.Second)); err != nil {
		t.Fatalf("pre-tag other-router: %v", err)
	} else if n != 1 {
		t.Fatalf("pre-tag tagged %d rows, want 1 (tx B)", n)
	}

	// ─── The real pass ──────────────────────────────────────────
	tagged, err := store.TagTradesRoutedVia(ctx, "soroswap-router", "soroswap", windowFrom, windowTo)
	if err != nil {
		t.Fatalf("TagTradesRoutedVia: %v", err)
	}
	// Tx A's two soroswap legs only: the phoenix same-tx row is
	// source-scoped out, tx B is first-wins-protected, `direct` has
	// no router call.
	if tagged != 2 {
		t.Errorf("tagged = %d, want 2 (tx A's two soroswap legs)", tagged)
	}

	// Idempotence: an identical re-run is a no-op.
	again, err := store.TagTradesRoutedVia(ctx, "soroswap-router", "soroswap", windowFrom, windowTo)
	if err != nil {
		t.Fatalf("TagTradesRoutedVia rerun: %v", err)
	}
	if again != 0 {
		t.Errorf("rerun tagged = %d, want 0 (idempotent)", again)
	}

	// Read back through the /v1/history read path.
	trades, err := store.TradesInRange(ctx, pair, windowFrom, windowTo.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	byKey := map[string]string{} // "<ledger>/<op>" → routed_via
	for _, tr := range trades {
		if tr.Source == "soroswap" {
			byKey[keyOf(tr.Ledger, tr.OpIndex)] = tr.RoutedVia
		}
	}
	if got := byKey[keyOf(100, txA.OpIndex)]; got != "soroswap-router" {
		t.Errorf("tx A leg 1 routed_via = %q, want soroswap-router", got)
	}
	if got := byKey[keyOf(100, txA2.OpIndex)]; got != "soroswap-router" {
		t.Errorf("tx A leg 2 routed_via = %q, want soroswap-router", got)
	}
	if got := byKey[keyOf(101, txB.OpIndex)]; got != "other-router" {
		t.Errorf("tx B routed_via = %q, want other-router (first-wins violated)", got)
	}
	if got := byKey[keyOf(102, direct.OpIndex)]; got != "" {
		t.Errorf("direct trade routed_via = %q, want empty", got)
	}

	// ─── Rollup math ────────────────────────────────────────────
	rollup, err := store.AggregatorRollup(ctx, windowFrom)
	if err != nil {
		t.Fatalf("AggregatorRollup: %v", err)
	}
	var router *timescale.AggregatorRollupRow
	vaults := 0
	for i := range rollup {
		switch rollup[i].Kind {
		case "router":
			if rollup[i].Name == "soroswap-router" {
				router = &rollup[i]
			}
		case "aggregator-vault":
			vaults++
		}
	}
	// Migration 0072 must have renamed the 0032 seed; 0033 seeds 3 vaults.
	if router == nil {
		t.Fatalf("no 'soroswap-router' registry row in rollup (0032 seed + 0072 rename missing?): %+v", rollup)
	}
	if vaults != 3 {
		t.Errorf("vault rows = %d, want 3 (0033 seed)", vaults)
	}
	if router.RoutedTrades != 2 {
		t.Errorf("RoutedTrades = %d, want 2", router.RoutedTrades)
	}
	// No usd_volume on any fixture trade → NULL volume, not "0".
	if router.RoutedVolume != nil {
		t.Errorf("RoutedVolume = %v, want nil (no USD valuation)", *router.RoutedVolume)
	}
	if router.LastRoutedAt == nil || !router.LastRoutedAt.Equal(t0) {
		t.Errorf("LastRoutedAt = %v, want %v (max ts of routed trades)", router.LastRoutedAt, t0)
	}
	// Vault entries carry zero routed stats.
	for i := range rollup {
		if rollup[i].Kind == "aggregator-vault" && rollup[i].RoutedTrades != 0 {
			t.Errorf("vault %s RoutedTrades = %d, want 0", rollup[i].Name, rollup[i].RoutedTrades)
		}
	}

	// `since` bound: a window starting after tx A excludes its trades.
	late, err := store.AggregatorRollup(ctx, t0.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("AggregatorRollup(late): %v", err)
	}
	for i := range late {
		if late[i].Name == "soroswap-router" && late[i].RoutedTrades != 0 {
			t.Errorf("late-window RoutedTrades = %d, want 0", late[i].RoutedTrades)
		}
	}
}

func keyOf(ledger, op uint32) string {
	return fmt.Sprintf("%d/%d", ledger, op)
}
