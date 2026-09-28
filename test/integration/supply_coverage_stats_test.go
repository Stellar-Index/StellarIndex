//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestSupplyCoverageStats_CountsEveryAssetWithNoWindow pins the diagnostic's
// answer to all history: an asset last snapshotted a year ago still counts,
// and the newest snapshot supplies last_snapshot_at and latest_ledger.
func TestSupplyCoverageStats_CountsEveryAssetWithNoWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if got, err := store.SupplyCoverageStats(ctx); err != nil || got != (timescale.SupplyCoverage{}) {
		t.Fatalf("empty table: got %+v, %v; want zero value", got, err)
	}

	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	sep41 := "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	classicOld := "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	rows := []struct {
		key    string
		ledger uint32
		at     time.Time
	}{
		{"XLM", 59_000_000, now.Add(-24 * time.Hour)},
		{"XLM", 59_100_000, now},
		{sep41, 58_000_000, now.Add(-40 * 24 * time.Hour)},
		{classicOld, 53_000_000, now.Add(-365 * 24 * time.Hour)},
	}
	for _, r := range rows {
		if err := store.InsertSupply(ctx, supply.Supply{
			AssetKey:          r.key,
			TotalSupply:       big.NewInt(1),
			CirculatingSupply: big.NewInt(1),
			Basis:             supply.BasisXLMSDFReserveExclusion,
			LedgerSequence:    r.ledger,
			ObservedAt:        r.at,
		}); err != nil {
			t.Fatalf("InsertSupply %s@%d: %v", r.key, r.ledger, err)
		}
	}

	got, err := store.SupplyCoverageStats(ctx)
	if err != nil {
		t.Fatalf("SupplyCoverageStats: %v", err)
	}
	want := timescale.SupplyCoverage{
		ClassicAssets:  2, // XLM and the year-old USDC
		SEP41Assets:    1,
		LastSnapshotAt: now,
		LatestLedger:   59_100_000,
	}
	if got.ClassicAssets != want.ClassicAssets || got.SEP41Assets != want.SEP41Assets ||
		!got.LastSnapshotAt.Equal(want.LastSnapshotAt) || got.LatestLedger != want.LatestLedger {
		t.Fatalf("SupplyCoverageStats = %+v, want %+v", got, want)
	}
}
