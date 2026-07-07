//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// liveAsset24hVolSQL is the pre-#43 single-asset per-request SUM the
// per_asset_24h_vol CTE used to inline. The rollup must reproduce it
// byte-for-byte (only NUMERIC, ADR-0003) — the #43 change moved the
// compute off the request path, not the value.
const liveAsset24hVolSQL = `
SELECT COALESCE(SUM(volume_usd), 0)::text
  FROM (
    SELECT volume_usd FROM prices_1m
     WHERE base_asset = $1
       AND bucket >= now() - INTERVAL '24 hours'
       AND bucket  <  now()
       AND volume_usd IS NOT NULL
    UNION ALL
    SELECT volume_usd FROM prices_1m
     WHERE quote_asset = $1
       AND bucket >= now() - INTERVAL '24 hours'
       AND bucket  <  now()
       AND volume_usd IS NOT NULL
  ) t`

// TestAssetVolumeRollup_MatchesLiveSum proves the #43 rollup end-to-end:
// RefreshAssetVolume24h populates asset_volume_24h with values that are
// byte-identical to the old inline per-request SUM over prices_1m, the
// refresh is idempotent, and an asset that ages out of the 24h window is
// pruned on the next pass.
func TestAssetVolumeRollup_MatchesLiveSum(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlm, _ := c.NewCryptoAsset("XLM")
	usd, _ := c.NewFiatAsset("USD")
	xlmUSD, _ := c.NewPair(xlm, usd)

	// binance + fiat:USD trades carry a populated usd_volume, so
	// prices_1m.volume_usd is > 0 for this pair. Two trades in the same
	// minute → one bucket whose volume_usd sums both.
	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	seed := []c.Trade{
		mkIntegrationTrade("binance", 1, ts, xlmUSD, 100_000_000, 12_000_000),
		mkIntegrationTrade("binance", 2, ts, xlmUSD, 100_000_000, 34_000_000),
	}
	for _, tr := range seed {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", tr.Source, err)
		}
	}

	// Materialize prices_1m so the rollup + the live comparison see rows.
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	if err := store.RefreshAssetVolume24h(ctx); err != nil {
		t.Fatalf("RefreshAssetVolume24h: %v", err)
	}

	// Every rollup row must equal the live single-asset SUM for that
	// asset, rendered identically (the byte-identical guarantee).
	rows, err := store.DB().QueryContext(ctx, `SELECT asset_id, vol_usd::text FROM asset_volume_24h`)
	if err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	seen := 0
	for rows.Next() {
		var assetID, rollupVal string
		if err := rows.Scan(&assetID, &rollupVal); err != nil {
			t.Fatalf("scan rollup: %v", err)
		}
		var liveVal string
		if err := store.DB().QueryRowContext(ctx, liveAsset24hVolSQL, assetID).Scan(&liveVal); err != nil {
			t.Fatalf("live sum for %s: %v", assetID, err)
		}
		if rollupVal != liveVal {
			t.Errorf("asset %s: rollup vol_usd=%q, live SUM=%q (must be byte-identical)", assetID, rollupVal, liveVal)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	// Both sides of the pair (base XLM + quote fiat:USD) should be
	// present, so the test actually exercised a populated rollup.
	if seen < 2 {
		t.Fatalf("rollup has %d rows, want >= 2 (base + quote of the seeded pair)", seen)
	}

	// Idempotent: a second refresh leaves the values unchanged.
	if err := store.RefreshAssetVolume24h(ctx); err != nil {
		t.Fatalf("RefreshAssetVolume24h (2nd): %v", err)
	}
	var after int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM asset_volume_24h`).Scan(&after); err != nil {
		t.Fatalf("count after 2nd refresh: %v", err)
	}
	if after != seen {
		t.Errorf("row count changed across idempotent refresh: %d → %d", seen, after)
	}

	// Prune: a stale sentinel row (an asset with no volume this pass)
	// must be dropped by the next refresh, while the live rows survive.
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO asset_volume_24h (asset_id, vol_usd, computed_at)
		 VALUES ('ZZZ-STALE-ASSET', 12345, now() - interval '1 hour')`); err != nil {
		t.Fatalf("insert stale sentinel: %v", err)
	}
	if err := store.RefreshAssetVolume24h(ctx); err != nil {
		t.Fatalf("RefreshAssetVolume24h (3rd): %v", err)
	}
	var stale int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM asset_volume_24h WHERE asset_id = 'ZZZ-STALE-ASSET'`).Scan(&stale); err != nil {
		t.Fatalf("count sentinel after prune: %v", err)
	}
	if stale != 0 {
		t.Errorf("stale sentinel still present after refresh, want pruned")
	}
	var live int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM asset_volume_24h`).Scan(&live); err != nil {
		t.Fatalf("count live after prune: %v", err)
	}
	if live != seen {
		t.Errorf("live rollup rows changed across prune: %d → %d", seen, live)
	}
}
