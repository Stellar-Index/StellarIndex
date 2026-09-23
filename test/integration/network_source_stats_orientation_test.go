//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestGetSourceStatsFoldsFlippedOrientation pins RLT-274: a source
// that printed the same market in both stored orientations (native/USDC
// and USDC/native) must be counted as ONE market, not two. Before the
// fix, GetSourceStats grouped its per_pair CTE on the raw (base_asset,
// quote_asset) columns with no canonical-orientation fold, so
// MarketsCount24h double-counted a flipped pair.
func TestGetSourceStatsFoldsFlippedOrientation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	now := time.Now().UTC()

	// Same source, same market, opposite stored orientations, both
	// inside the 24h window.
	seedGrainTrade(t, ctx, db, 1, "sdex", now.Add(-30*time.Minute), "native", usdc, "1", "0.5", "1")
	seedGrainTrade(t, ctx, db, 2, "sdex", now.Add(-20*time.Minute), usdc, "native", "0.5", "1", "1")

	stats, err := store.GetSourceStats(ctx)
	if err != nil {
		t.Fatalf("GetSourceStats: %v", err)
	}
	var row *timescale.SourceStats
	for i := range stats {
		if stats[i].Source == "sdex" {
			row = &stats[i]
		}
	}
	if row == nil {
		t.Fatalf("sdex missing from GetSourceStats: %+v", stats)
	}
	if row.MarketsCount24h != 1 {
		t.Errorf("sdex MarketsCount24h = %d, want 1 (native/USDC and USDC/native are the same market)", row.MarketsCount24h)
	}
	if row.TradeCount24h != 2 {
		t.Errorf("sdex TradeCount24h = %d, want 2 (both prints still counted)", row.TradeCount24h)
	}
}

// TestGetNetworkStatsFoldsFlippedOrientation pins the same defect
// (RLT-274) in GetNetworkStats's MarketsCount24h: the DISTINCT over
// prices_1m must fold a market's two stored orientations before
// counting.
func TestGetNetworkStatsFoldsFlippedOrientation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	now := time.Now().UTC()

	seedGrainTrade(t, ctx, db, 101, "sdex", now.Add(-30*time.Minute), "native", usdc, "1", "0.5", "1")
	seedGrainTrade(t, ctx, db, 102, "soroswap", now.Add(-20*time.Minute), usdc, "native", "0.5", "1", "1")

	if _, err := db.ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
	); err != nil {
		t.Fatalf("refresh cagg prices_1m: %v", err)
	}

	before, err := store.GetNetworkStats(ctx)
	if err != nil {
		t.Fatalf("GetNetworkStats: %v", err)
	}
	if before.MarketsCount24h != 1 {
		t.Errorf("MarketsCount24h = %d, want 1 (native/USDC on two venues, both orientations, is one market)", before.MarketsCount24h)
	}
}

// TestGetNetworkStatsLatestLedgerReadsOnlyLiveCursors pins T603: the
// home page's latest_ledger must be the live tip, not the highest
// ledger any one-shot job's shard cursor ever reached. Each job writes
// its own namespace ("census-backfill", "projected-rebuild", …), and a
// denylist of the literal "backfill" counted every one of them as live.
func TestGetNetworkStatsLatestLedgerReadsOnlyLiveCursors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cursors := []struct {
		source, sub string
		ledger      uint32
	}{
		{"ledgerstream", "", 62_000_000},
		{"projector", "blend", 61_999_990},
		{"backfill", "0-70000000:sdex", 70_000_000},
		{"census-backfill", "shard-3", 69_000_000},
		{"projected-rebuild", "soroswap:60000000-68000000", 68_000_000},
		{"tag-signer", "shard-1", 67_000_000},
	}
	for _, c := range cursors {
		if err := store.UpsertCursor(ctx, c.source, c.sub, c.ledger); err != nil {
			t.Fatalf("UpsertCursor(%s,%s): %v", c.source, c.sub, err)
		}
	}

	got, err := store.GetNetworkStats(ctx)
	if err != nil {
		t.Fatalf("GetNetworkStats: %v", err)
	}
	if got.LatestLedger != 62_000_000 {
		t.Errorf("LatestLedger = %d, want 62000000 (the ledgerstream tip; one-shot job cursors are not live)", got.LatestLedger)
	}
}
