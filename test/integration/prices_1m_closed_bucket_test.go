//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPriceCAGGsAreMaterializedOnly pins the setting most prices_1m readers
// rely on to exclude the in-progress bucket: with real-time aggregation off,
// that bucket is never materialized. Enabling it (as 0069/0076 did for
// sibling CAGGs) must be a deliberate change that audits every reader.
func TestPriceCAGGsAreMaterializedOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rows, err := store.DB().QueryContext(ctx, `
		SELECT view_name, materialized_only
		  FROM timescaledb_information.continuous_aggregates
		 WHERE view_name LIKE 'prices\_%' OR view_name LIKE 'twap\_%'`)
	if err != nil {
		t.Fatalf("query continuous_aggregates: %v", err)
	}
	defer func() { _ = rows.Close() }()
	seen := 0
	for rows.Next() {
		var name string
		var matOnly bool
		if err := rows.Scan(&name, &matOnly); err != nil {
			t.Fatal(err)
		}
		seen++
		if !matOnly {
			t.Errorf("%s has real-time aggregation enabled; its readers would serve the in-progress bucket", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Seven prices_* grains + twap_1h/twap_1d; fewer means the filter missed.
	if seen < 9 {
		t.Fatalf("checked %d price CAGGs, want >= 9", seen)
	}
}

// TestTrailing24hVolume_ExcludesInProgressMinute enables real-time
// aggregation on prices_1m so the current minute is visible, then asserts
// the trailing-24h volume readers sum only CLOSED buckets (ADR-0015).
// A `bucket < now()` ceiling admits the current minute and fails this.
func TestTrailing24hVolume_ExcludesInProgressMinute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cryptoXLM, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(cryptoXLM, usd)

	// Keep the live trade's minute open for the whole assertion window.
	if s := time.Now().UTC().Second(); s >= 40 {
		time.Sleep(time.Duration(61-s) * time.Second)
	}
	now := time.Now().UTC()
	closedTS := now.Add(-10 * time.Minute).Truncate(time.Minute)
	for _, tr := range []c.Trade{
		mkIntegrationTrade("binance", 1, closedTS, pair, 100_000_000, 30_000_000),
		mkIntegrationTrade("binance", 2, now, pair, 500_000_000, 150_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`ALTER MATERIALIZED VIEW prices_1m SET (timescaledb.materialized_only = false)`); err != nil {
		t.Fatalf("enable real-time aggregation: %v", err)
	}

	asset := cryptoXLM.String()
	closedOnly, all := closedAndLiveVolume(t, ctx, store, asset, closedTS)
	if !(closedOnly > 0 && all > closedOnly) {
		t.Fatalf("fixture did not surface a live minute: closed=%.4f all=%.4f", closedOnly, all)
	}

	got := map[string]string{}
	if got["Volume24hUSDForAsset"], err = store.Volume24hUSDForAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	if got["SorobanVolume24hUSDForAsset"], err = store.SorobanVolume24hUSDForAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	row, err := store.LatestAssetStats(ctx, asset)
	if err != nil {
		t.Fatal(err)
	}
	if row.Volume24hUSD == nil {
		t.Fatal("LatestAssetStats.Volume24hUSD = nil")
	}
	got["LatestAssetStats"] = *row.Volume24hUSD

	if time.Now().UTC().Truncate(time.Minute) != now.Truncate(time.Minute) {
		t.Fatalf("the live minute closed mid-test (started %s); the assertion window is invalid", now)
	}
	for name, v := range got {
		if f := mustFloat(t, v); f < closedOnly-1e-6 || f > closedOnly+1e-6 {
			t.Errorf("%s = %s, want closed-bucket-only %.6f (in-progress minute included: %.6f)",
				name, v, closedOnly, all)
		}
	}
}

// closedAndLiveVolume reads prices_1m's USD volume for the asset in the one
// closed fixture bucket and across every bucket, including the live one.
func closedAndLiveVolume(t *testing.T, ctx context.Context, store *timescale.Store, asset string, closedBucket time.Time) (float64, float64) {
	t.Helper()
	var closedOnly, all float64
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COALESCE(sum(volume_usd) FILTER (WHERE bucket = $2), 0)::float8,
		       COALESCE(sum(volume_usd), 0)::float8
		  FROM prices_1m
		 WHERE base_asset = $1 OR quote_asset = $1`, asset, closedBucket).Scan(&closedOnly, &all); err != nil {
		t.Fatalf("ground-truth volume: %v", err)
	}
	return closedOnly, all
}
