//go:build integration

package integration_test

import (
	"context"
	"errors"
	"math/big"
	"slices"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestOracleCAGGsMatchCatalog is TestTradesCAGGsMatchCatalog for the
// oracle_updates root: every aggregate built (at any depth) on
// oracle_updates is in timescale.OracleCAGGs, which backfill refreshes
// after each chunk, and nothing else is (GH-687).
func TestOracleCAGGsMatchCatalog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	inCatalog := catalogNames(t, ctx, store, `
		WITH RECURSIVE chain AS (
		    SELECT ca.user_view_name, ca.raw_hypertable_id, ca.parent_mat_hypertable_id
		      FROM _timescaledb_catalog.continuous_agg ca
		    UNION ALL
		    SELECT c.user_view_name, p.raw_hypertable_id, p.parent_mat_hypertable_id
		      FROM chain c
		      JOIN _timescaledb_catalog.continuous_agg p ON p.mat_hypertable_id = c.parent_mat_hypertable_id
		)
		SELECT DISTINCT c.user_view_name
		  FROM chain c
		  JOIN _timescaledb_catalog.hypertable h ON h.id = c.raw_hypertable_id
		 WHERE c.parent_mat_hypertable_id IS NULL
		   AND h.table_name = 'oracle_updates'
		 ORDER BY 1`)
	var listed []string
	for _, spec := range timescale.OracleCAGGs {
		listed = append(listed, spec.Name)
		if !timescale.IsRefreshableCAGG(spec.Name) {
			t.Errorf("%s is in OracleCAGGs but RefreshContinuousAggregate refuses it", spec.Name)
		}
	}
	slices.Sort(listed)
	if len(inCatalog) == 0 || !slices.Equal(inCatalog, listed) {
		t.Errorf("oracle_updates-rooted aggregates in the schema: %v, timescale.OracleCAGGs: %v", inCatalog, listed)
	}
}

// TestOracleCAGGsRefreshOverABackfilledRange is the oracle half of
// GH-687 on a real TimescaleDB: oracle rows written for a historical
// ledger range are invisible to oracle_prices_1d (created WITH NO DATA,
// policy reach 7 days) until refreshed, and the store now finds that
// range's time span and accepts every oracle_prices_* view.
func TestOracleCAGGsRefreshOverABackfilledRange(t *testing.T) {
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
	day1 := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)
	for i, ts := range []time.Time{day1, day1.Add(24 * time.Hour)} {
		u := c.OracleUpdate{
			Source: "reflector-dex", Ledger: uint32(50_000_001 + i),
			TxHash:    "3333333333333333333333333333333333333333333333333333333333333333",
			Timestamp: ts, Asset: c.NativeAsset(), Quote: usdc,
			Price: c.NewAmount(big.NewInt(int64(1_000_000 + i))), Decimals: 14,
		}
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("InsertOracleUpdate: %v", err)
		}
	}
	if _, _, err := store.LedgerRangeToOracleTimeRange(ctx, 1, 2); !errors.Is(err, timescale.ErrNotFound) {
		t.Fatalf("empty ledger range: err = %v, want ErrNotFound", err)
	}
	from, to, err := store.LedgerRangeToOracleTimeRange(ctx, 50_000_000, 50_000_010)
	if err != nil {
		t.Fatalf("LedgerRangeToOracleTimeRange: %v", err)
	}
	if !from.Equal(day1) || !to.Equal(day1.Add(24*time.Hour)) {
		t.Fatalf("span = [%s, %s], want [%s, %s]", from, to, day1, day1.Add(24*time.Hour))
	}

	readDays := func() int {
		t.Helper()
		pts, err := store.DailyOraclePrices(ctx, []c.Asset{c.NativeAsset()}, usdc,
			day1.Truncate(24*time.Hour), day1.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("DailyOraclePrices: %v", err)
		}
		return len(pts)
	}
	if n := readDays(); n != 0 {
		t.Fatalf("oracle_prices_1d served %d day(s) before any refresh; this test's premise (materialised-only, WITH NO DATA) no longer holds", n)
	}
	for _, spec := range timescale.OracleCAGGs {
		f, tt := timescale.PadRefreshWindow(from, to, spec.MinWindow)
		if err := store.RefreshContinuousAggregate(ctx, spec.Name, f, tt); err != nil {
			t.Fatalf("refresh %s: %v", spec.Name, err)
		}
	}
	if n := readDays(); n != 2 {
		t.Fatalf("oracle_prices_1d serves %d day(s) after the refresh, want 2", n)
	}
}
