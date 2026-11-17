//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A sub-cent print must not set the listing's headline price or change pills while a minute over the $0.01 notional floor exists:
// the arm pick ranks a floored arm over a newer crumb-only one, and a crumb lookback is no baseline for a floored headline.
func TestAssetPriceSnapshot_NotionalFloorAcrossArms(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	usdcID := "USDC-" + zeroLegIssuer
	xlmArm := "TARM-" + zeroLegIssuer  // real XLM market, crumb-only USDC market
	pillArm := "TPIL-" + zeroLegIssuer // real USDC market, crumb-only 1h lookback
	mirror := "TMIR-" + zeroLegIssuer  // real USDC market, crumb-only XLM market
	now := time.Now().UTC().Truncate(time.Minute)

	// XLM anchor at 0.30 USD; TARM at 1/3 XLM = 0.10 USD ten minutes ago.
	seed(t, db, ctx, ohlcDustPair{base: "native", quote: usdcID}, []seedTrade{
		{off: 0, base: "1000000000", quote: "300000000", usd: "30"},
	}, now.Add(-10*time.Minute))
	seed(t, db, ctx, ohlcDustPair{base: xlmArm, quote: "native"}, []seedTrade{
		{off: 0, base: "300000000", quote: "100000000", usd: "3"},
	}, now.Add(-10*time.Minute))
	// 1 stroop of TARM for 0.009 USDC: 90000 USD per unit, under the floor.
	seed(t, db, ctx, ohlcDustPair{base: xlmArm, quote: usdcID}, []seedTrade{
		{off: 0, base: "1", quote: "90000", usd: "0.009"},
	}, now.Add(-2*time.Minute))

	// TPIL at 0.10 USD two minutes ago; the only trade an hour ago is 100 TPIL for 1 stroop of USDC.
	seed(t, db, ctx, ohlcDustPair{base: pillArm, quote: usdcID}, []seedTrade{
		{off: 0, base: "100000000", quote: "10000000", usd: "1"},
	}, now.Add(-2*time.Minute))
	seed(t, db, ctx, ohlcDustPair{base: pillArm, quote: usdcID}, []seedTrade{
		{off: 0, base: "1000000000", quote: "1", usd: "0.0000001"},
	}, now.Add(-60*time.Minute))

	// Mirror: TMIR at 0.10 USD forty minutes ago, then a crumb on its XLM book at 50 XLM (15 USD).
	seed(t, db, ctx, ohlcDustPair{base: mirror, quote: usdcID}, []seedTrade{
		{off: 0, base: "100000000", quote: "10000000", usd: "1"},
	}, now.Add(-40*time.Minute))
	seed(t, db, ctx, ohlcDustPair{base: mirror, quote: "native"}, []seedTrade{
		{off: 0, base: "1", quote: "50", usd: "0.0000015"},
	}, now.Add(-2*time.Minute))

	if _, err := db.ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	if err := store.RefreshAssetListingRollups(ctx); err != nil {
		t.Fatalf("RefreshAssetListingRollups: %v", err)
	}

	read := func(asset string) (price string, priceIsTenth bool, change1h sql.NullString) {
		t.Helper()
		if err := db.QueryRowContext(ctx,
			`SELECT price_usd::text, round(price_usd, 9) = 0.1, change_1h_pct::text
			   FROM asset_price_snapshot WHERE asset_id = $1`, asset).Scan(&price, &priceIsTenth, &change1h); err != nil {
			t.Fatalf("read asset_price_snapshot %s: %v", asset, err)
		}
		return price, priceIsTenth, change1h
	}

	t.Run("floored XLM arm beats newer crumb USD arm", func(t *testing.T) {
		if price, ok, _ := read(xlmArm); !ok {
			t.Errorf("price_usd = %s, want 0.1 from the floored XLM arm", price)
		}
	})
	t.Run("floored USD arm beats newer crumb XLM arm", func(t *testing.T) {
		if price, ok, _ := read(mirror); !ok {
			t.Errorf("price_usd = %s, want 0.1 from the floored USD arm", price)
		}
	})
	t.Run("crumb lookback yields no change pill", func(t *testing.T) {
		price, ok, change := read(pillArm)
		if !ok {
			t.Errorf("price_usd = %s, want 0.1", price)
		}
		if change.Valid {
			t.Errorf("change_1h_pct = %s, want NULL against a sub-cent lookback", change.String)
		}
	})
}
