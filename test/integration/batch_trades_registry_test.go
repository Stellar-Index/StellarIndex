//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestBatchInsertTrades_PopulatesClassicAssetRegistry is the C2-13b proof:
// the LIVE indexer ingests trades EXCLUSIVELY through BatchInsertTrades
// (persistWorker → batch path), which previously SKIPPED the classic-asset
// / issuer registry hook that the single-row InsertTrade path runs. The
// result: classic_assets + issuers permanently under-populated for every
// batch-ingested asset.
//
// This test drives a classic-asset trade through BOTH paths and asserts the
// registry lands identically. Before the fix, the batch-path assertions
// (classic_assets row present, issuers row present) fail with "no rows" —
// the registry hook never ran on the batch path.
func TestBatchInsertTrades_PopulatesClassicAssetRegistry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuerG = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", issuerG)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	timescale.ResetAssetRegistryDedupeForTest()
	ts := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)

	mkTrade := func(ledger uint32, txTail rune) c.Trade {
		txHash := "beadfeedbeadfeedbeadfeedbeadfeedbeadfeedbeadfeedbeadfeedbeadfe" + string(txTail)
		return c.Trade{
			Source:      "test-batch-registry",
			Ledger:      ledger,
			TxHash:      txHash,
			OpIndex:     0,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
		}
	}

	// Batch-insert two distinct trades on the same classic asset in one call.
	batch := []c.Trade{
		mkTrade(60_000_000, 'a'),
		mkTrade(60_000_001, 'b'),
	}
	if err := store.BatchInsertTrades(ctx, batch); err != nil {
		t.Fatalf("BatchInsertTrades: %v", err)
	}

	// classic_assets must now carry the asset (C2-13b): before the fix this
	// row did not exist and readRegistry fatals with "no rows".
	gotCount, gotLastLedger := readRegistry(t, store, usdc.String())
	if gotCount == 0 {
		t.Fatalf("classic_assets observation_count = 0, want >= 1 — batch path skipped the registry hook (C2-13b regression)")
	}
	// The two distinct landed trades are deduped to ONE registry upsert per
	// asset within the batch (dedupe-cached), advancing last_seen to the
	// highest ledger observed.
	if gotLastLedger != 60_000_001 {
		t.Errorf("classic_assets last_seen_ledger = %d, want 60_000_001 (highest landed ledger)", gotLastLedger)
	}

	// issuers must carry the issuer G-strkey too.
	if n := countIssuers(t, store, issuerG); n != 1 {
		t.Errorf("issuers rows for %s = %d, want 1 — batch path skipped registerIssuerSeen", issuerG, n)
	}

	// A replay of the SAME batch (cold dedupe cache = simulated restart) must
	// NOT inflate observation_count: the hook only fires for genuinely-landed
	// (xmax=0) rows, matching the single-row path's F-1243 guard.
	timescale.ResetAssetRegistryDedupeForTest()
	if err := store.BatchInsertTrades(ctx, batch); err != nil {
		t.Fatalf("BatchInsertTrades (replay): %v", err)
	}
	replayCount, _ := readRegistry(t, store, usdc.String())
	if replayCount != gotCount {
		t.Errorf("after replay: observation_count = %d, want %d (duplicate batch must not advance the registry)", replayCount, gotCount)
	}
}

// countIssuers returns the number of issuers rows for the given G-strkey.
func countIssuers(t *testing.T, store *timescale.Store, gStrkey string) int {
	t.Helper()
	const q = `SELECT COUNT(*) FROM issuers WHERE g_strkey = $1`
	var n int
	if err := store.DB().QueryRow(q, gStrkey).Scan(&n); err != nil {
		t.Fatalf("countIssuers %s: %v", gStrkey, err)
	}
	return n
}
