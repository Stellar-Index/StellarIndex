//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const zeroLegIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestPriceReadersOnZeroLegBuckets runs every prices_* reader that combines
// vwap with a volume over buckets holding zero-leg trades (migration 0187).
// vwap covers only trades with both legs > 0, so a price must be weighted by
// volume_priced and a quote volume read from volume_quote; `vwap * volume`
// counts a zero-quote trade's base at the bucket price and drops a zero-base
// trade's quote.
//
// ZLEG/USDC, all amounts in 1e7 units:
//
//	H1 (t0+10s)  ZLEG/USDC 10/20 · 30/0     USDC/ZLEG 40/10 · 0/5
//	H2 (t0+1h)   ZLEG/USDC 10/40 · 5/0      ZLEG/USDC-SAC 20/20
//
// H1's priceable union is 20 ZLEG for 60 USDC = 3 (2.4 weighted by volume).
func TestPriceReadersOnZeroLegBuckets(t *testing.T) {
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
	for _, stmt := range []string{
		`ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_base_amount_check`,
		`ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_quote_amount_check`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	const usdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	usdcID := "USDC-" + zeroLegIssuer
	zleg := ohlcDustPair{base: "ZLEG-" + zeroLegIssuer, quote: usdcID}
	flip := ohlcDustPair{base: usdcID, quote: zleg.base}
	viaSAC := ohlcDustPair{base: zleg.base, quote: usdcSAC}
	dust := ohlcDustPair{base: "ZDST-" + zeroLegIssuer, quote: usdcID}

	day := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour)
	h1, h2 := day.Add(time.Hour), day.Add(2*time.Hour)
	seed(t, db, ctx, zleg, []seedTrade{
		{off: 10 * time.Second, base: "100000000", quote: "200000000", usd: "20"},
		{off: 20 * time.Second, base: "300000000", quote: "0", usd: "1"},
	}, h1)
	seed(t, db, ctx, flip, []seedTrade{
		{off: 30 * time.Second, base: "400000000", quote: "100000000", usd: "40"},
		{off: 40 * time.Second, base: "0", quote: "50000000", usd: "1"},
	}, h1)
	seed(t, db, ctx, zleg, []seedTrade{
		{off: 10 * time.Second, base: "100000000", quote: "400000000", usd: "40"},
		{off: 20 * time.Second, base: "50000000", quote: "0", usd: "1"},
	}, h2)
	seed(t, db, ctx, viaSAC, []seedTrade{
		{off: 30 * time.Second, base: "200000000", quote: "200000000", usd: "20"},
	}, h2)
	// ZDST: a real bucket at 2, then a newer one whose only priceable fill
	// is one stroop each way beside a whole-unit zero-quote trade.
	seed(t, db, ctx, dust, []seedTrade{
		{off: 10 * time.Second, base: "100000000", quote: "200000000", usd: "20"},
	}, h1)
	seed(t, db, ctx, dust, []seedTrade{
		{off: 10 * time.Second, base: "1", quote: "1", usd: ""},
		{off: 20 * time.Second, base: "10000000", quote: "0", usd: ""},
	}, h1.Add(30*time.Minute))
	for _, v := range []string{"prices_1m", "prices_1h", "prices_1mo"} {
		if _, err := db.ExecContext(ctx, "CALL refresh_continuous_aggregate('"+v+"', NULL, NULL)"); err != nil {
			t.Fatalf("refresh %s: %v", v, err)
		}
	}

	usdc, err := c.NewClassicAsset("USDC", zeroLegIssuer)
	if err != nil {
		t.Fatal(err)
	}
	zlegAsset, err := c.NewClassicAsset("ZLEG", zeroLegIssuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(zlegAsset, usdc)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("OHLCSeries", func(t *testing.T) {
		bars, err := store.OHLCSeries(ctx, pair, timescale.Granularity1h, h1, h1.Add(time.Hour), 10)
		if err != nil {
			t.Fatalf("OHLCSeries: %v", err)
		}
		if len(bars) != 1 {
			t.Fatalf("bars = %d, want 1", len(bars))
		}
		// base = 10+30 stored + 10+5 flipped quote; quote = 20+0 + 40+0 flipped base.
		if bars[0].BaseVolume != "550000000" || bars[0].QuoteVolume != "600000000" {
			t.Errorf("base/quote volume = %s/%s, want 550000000/600000000", bars[0].BaseVolume, bars[0].QuoteVolume)
		}
	})
	t.Run("OHLCSeriesReBucketed", func(t *testing.T) {
		bars, err := store.OHLCSeriesReBucketed(ctx, pair, timescale.Granularity1h, "4 hours", day, day.Add(4*time.Hour), 10)
		if err != nil {
			t.Fatalf("OHLCSeriesReBucketed: %v", err)
		}
		if len(bars) != 1 {
			t.Fatalf("bars = %d, want 1", len(bars))
		}
		if bars[0].BaseVolume != "700000000" || bars[0].QuoteVolume != "1000000000" {
			t.Errorf("base/quote volume = %s/%s, want 700000000/1000000000", bars[0].BaseVolume, bars[0].QuoteVolume)
		}
	})
	t.Run("HistoryPoints", func(t *testing.T) {
		pts, err := store.HistoryPoints(ctx, pair, timescale.Granularity1h, 0)
		if err != nil {
			t.Fatalf("HistoryPoints: %v", err)
		}
		if len(pts) == 0 || !pts[0].Bucket.Equal(h1) || pts[0].VWAP != "3" {
			t.Errorf("first point = %+v, want bucket %s vwap 3", pts, h1)
		}
	})
	t.Run("TimedVWAPsForPair1m", func(t *testing.T) {
		pts, err := store.TimedVWAPsForPair1m(ctx, pair, h1, h1.Add(time.Minute))
		if err != nil {
			t.Fatalf("TimedVWAPsForPair1m: %v", err)
		}
		if len(pts) != 1 || pts[0].VWAP != 3 {
			t.Errorf("points = %+v, want one at 3", pts)
		}
	})
	t.Run("TimedVWAPs1mForChangeSummary", func(t *testing.T) {
		pts, err := store.TimedVWAPs1mForChangeSummary(ctx, pair, h1, h1.Add(time.Minute))
		if err != nil {
			t.Fatalf("TimedVWAPs1mForChangeSummary: %v", err)
		}
		if len(pts) != 1 || pts[0].Value != "3" {
			t.Errorf("points = %+v, want one at 3", pts)
		}
	})
	t.Run("DailyMarketDays", func(t *testing.T) {
		// H1 at 2 and H2 at 4 on 10 priced units each; `volume` would
		// weight them 40:15.
		days, err := store.DailyMarketDays(ctx, []c.Asset{zlegAsset}, []c.Asset{usdc}, day, day)
		if err != nil {
			t.Fatalf("DailyMarketDays: %v", err)
		}
		if len(days) != 1 {
			t.Fatalf("days = %d, want 1", len(days))
		}
		var ok bool
		if err := db.QueryRowContext(ctx, `SELECT $1::numeric = 3`, days[0].VWAP).Scan(&ok); err != nil || !ok {
			t.Errorf("day vwap = %s, want 3 (err %v)", days[0].VWAP, err)
		}
	})
	t.Run("MonthlyUSDVWAPs", func(t *testing.T) {
		// 60 USDC / 20 ZLEG and 20 USDC-SAC / 20 ZLEG fold to 80/40.
		month := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
		rows, err := store.MonthlyUSDVWAPs(ctx, month, month.AddDate(0, 1, 0),
			func(src string) int { return external.Lookup(src).AmountScaleDecimals() })
		if err != nil {
			t.Fatalf("MonthlyUSDVWAPs: %v", err)
		}
		found := false
		for _, r := range rows {
			if r.Asset == zleg.base {
				found = true
				if r.VWAPUSD != "2" {
					t.Errorf("ZLEG monthly vwap = %s, want 2", r.VWAPUSD)
				}
			}
		}
		if !found {
			t.Errorf("no ZLEG row in %+v", rows)
		}
	})
	t.Run("AssetPriceSnapshot", func(t *testing.T) {
		// H2's minute: 10 ZLEG for 40 USDC and 20 for 20 USDC-SAC = 60/30.
		if err := store.RefreshAssetListingRollups(ctx); err != nil {
			t.Fatalf("RefreshAssetListingRollups: %v", err)
		}
		var price string
		var ok bool
		if err := db.QueryRowContext(ctx,
			`SELECT price_usd::text, price_usd = 2 FROM asset_price_snapshot WHERE asset_id = $1`,
			zleg.base).Scan(&price, &ok); err != nil {
			t.Fatalf("read asset_price_snapshot: %v", err)
		}
		if !ok {
			t.Errorf("price_usd = %s, want 2", price)
		}
	})
	t.Run("USDPriceAt dust floor", func(t *testing.T) {
		resolver, err := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
			USDPegs:   []string{usdcID},
			Freshness: -1,
			// Two buckets are no market; this pins the dust floor, not the gate.
			DisableSubstanceGate: true,
		})
		if err != nil {
			t.Fatalf("NewVWAPUSDFXResolver: %v", err)
		}
		zdst, err := c.NewClassicAsset("ZDST", zeroLegIssuer)
		if err != nil {
			t.Fatal(err)
		}
		// The newer bucket's priced notional is one stroop; its zero-quote
		// trade must not lift it over the one-cent floor.
		got, ok, err := resolver.USDPriceAt(ctx, zdst, h1.Add(40*time.Minute))
		r, parsed := new(big.Rat).SetString(got)
		if err != nil || !ok || !parsed || r.Cmp(big.NewRat(2, 1)) != 0 {
			t.Errorf("USDPriceAt = %q ok=%v err=%v, want 2 from the older real bucket", got, ok, err)
		}
	})
}
