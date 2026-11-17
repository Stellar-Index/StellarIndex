//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A folded OHLC bar takes its prices only from source buckets over the $0.01 notional floor whenever one exists, as the
// native views do over their whole bucket; volumes and counts still sum every source bucket.
func TestOHLCSeriesReBucketed_NotionalFloorAcrossFold(t *testing.T) {
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
	p := ohlcDustPair{base: "TFLD-" + zeroLegIssuer, quote: usdcID}
	day := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour)
	// Floored minutes at 0.30, 0.31, 0.29; crumb-only minutes (0.0001 TFLD each) at 50 first, 60 inside and 40 last.
	for _, tr := range []struct {
		minute     time.Duration
		quote, usd string
		base       string
	}{
		{0, "50000", "0.005", "1000"},
		{3, "300000000", "30", "1000000000"},
		{7, "60000", "0.006", "1000"},
		{15, "310000000", "31", "1000000000"},
		{20, "290000000", "29", "1000000000"},
		{29, "40000", "0.004", "1000"},
	} {
		seed(t, db, ctx, p, []seedTrade{{off: 10 * time.Second, base: tr.base, quote: tr.quote, usd: tr.usd}},
			day.Add(tr.minute*time.Minute))
	}
	if _, err := db.ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	usdc, err := c.NewClassicAsset("USDC", zeroLegIssuer)
	if err != nil {
		t.Fatal(err)
	}
	tfld, err := c.NewClassicAsset("TFLD", zeroLegIssuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(tfld, usdc)
	if err != nil {
		t.Fatal(err)
	}
	bars, err := store.OHLCSeriesReBucketed(ctx, pair, timescale.Granularity1m, "30 minutes", day, day.Add(30*time.Minute), 10)
	if err != nil {
		t.Fatalf("OHLCSeriesReBucketed: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("bars = %d, want 1", len(bars))
	}
	b := bars[0]
	for _, f := range []struct{ name, got, want string }{
		{"open", b.Open, "0.30"}, {"high", b.High, "0.31"}, {"low", b.Low, "0.29"}, {"close", b.Close, "0.29"},
	} {
		got, ok1 := new(big.Rat).SetString(f.got)
		want, _ := new(big.Rat).SetString(f.want)
		if !ok1 || got.Cmp(want) != 0 {
			t.Errorf("%s = %q, want %s", f.name, f.got, f.want)
		}
	}
	if b.TradeCount != 6 || b.BaseVolume != "3000003000" {
		t.Errorf("trade_count/base_volume = %d/%s, want 6/3000003000 (crumbs still counted)", b.TradeCount, b.BaseVolume)
	}
}

// A sparkline point (24h hourly, 7d daily; single and batch reads) comes from a minute over the $0.01 notional floor whenever the
// period has one, so a newer sub-cent crumb on the USD or XLM book cannot draw a 10x / 1000x spike.
func TestAssetPriceHistory_NotionalFloor(t *testing.T) {
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
	direct := "TSPD-" + zeroLegIssuer // USDC book
	viaXLM := "TSPX-" + zeroLegIssuer // XLM book only
	h := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)

	// XLM at 0.30 USD; both tokens at 0.10 USD, then a crumb thirty minutes later at 1 USD (direct) and 100 XLM (via XLM).
	seed(t, db, ctx, ohlcDustPair{base: "native", quote: usdcID}, []seedTrade{
		{off: 0, base: "1000000000", quote: "300000000", usd: "30"},
	}, h.Add(5*time.Minute))
	seed(t, db, ctx, ohlcDustPair{base: direct, quote: usdcID}, []seedTrade{
		{off: 0, base: "100000000", quote: "10000000", usd: "1"},
	}, h.Add(10*time.Minute))
	seed(t, db, ctx, ohlcDustPair{base: direct, quote: usdcID}, []seedTrade{
		{off: 0, base: "1", quote: "1", usd: "0.0000001"},
	}, h.Add(40*time.Minute))
	seed(t, db, ctx, ohlcDustPair{base: viaXLM, quote: "native"}, []seedTrade{
		{off: 0, base: "300000000", quote: "100000000", usd: "3"},
	}, h.Add(10*time.Minute))
	seed(t, db, ctx, ohlcDustPair{base: viaXLM, quote: "native"}, []seedTrade{
		{off: 0, base: "1", quote: "100", usd: "0.000003"},
	}, h.Add(40*time.Minute))
	if _, err := db.ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	tenth := new(big.Rat).SetFrac64(1, 10)
	check := func(t *testing.T, pts []timescale.AssetPricePoint, at time.Time) {
		t.Helper()
		want := at.Format("2006-01-02T15:04:05Z")
		for _, pt := range pts {
			if pt.T != want {
				continue
			}
			if pt.P == nil {
				t.Fatalf("point %s has no price, want 0.1", want)
			}
			got, ok := new(big.Rat).SetString(*pt.P)
			if !ok || got.Cmp(tenth) != 0 {
				t.Errorf("point %s = %s, want 0.1 from the floored minute", want, *pt.P)
			}
			return
		}
		t.Fatalf("no point at %s in %d points", want, len(pts))
	}
	day := h.Truncate(24 * time.Hour)
	for _, asset := range []string{direct, viaXLM} {
		t.Run(asset[:4], func(t *testing.T) {
			pts, err := store.GetAssetPriceHistory24h(ctx, asset)
			if err != nil {
				t.Fatal(err)
			}
			check(t, pts, h)
			if pts, err = store.GetAssetPriceHistory7d(ctx, asset); err != nil {
				t.Fatal(err)
			}
			check(t, pts, day)
			batch, err := store.GetAssetsPriceHistory24hBatch(ctx, []string{asset})
			if err != nil {
				t.Fatal(err)
			}
			check(t, batch[asset], h)
			if batch, err = store.GetAssetsPriceHistory7dBatch(ctx, []string{asset}); err != nil {
				t.Fatal(err)
			}
			check(t, batch[asset], day)
		})
	}
}

// A sparkline prices XLM per period, not at the minute, so a later unstamped crumb (volume_usd = 0) must not displace the day's
// stamped trade over the notional floor.
func TestAssetPriceHistory_UnstampedMinuteIsNotFloored(t *testing.T) {
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
	direct := "TSUD-" + zeroLegIssuer
	viaXLM := "TSUX-" + zeroLegIssuer
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	nine, half1 := day.Add(9*time.Hour), day.Add(13*time.Hour+30*time.Minute)

	// 09:00: XLM at 0.30 USD, both tokens at 0.10 USD, all stamped. 13:30: unstamped crumbs at 1 USD and 100 XLM.
	seed(t, db, ctx, ohlcDustPair{base: "native", quote: usdcID}, []seedTrade{
		{off: 0, base: "1000000000", quote: "300000000", usd: "30"},
	}, nine)
	seed(t, db, ctx, ohlcDustPair{base: direct, quote: usdcID}, []seedTrade{
		{off: 0, base: "100000000", quote: "10000000", usd: "1"},
	}, nine)
	seed(t, db, ctx, ohlcDustPair{base: direct, quote: usdcID}, []seedTrade{
		{off: 0, base: "1", quote: "1", usd: ""},
	}, half1)
	seed(t, db, ctx, ohlcDustPair{base: viaXLM, quote: "native"}, []seedTrade{
		{off: 0, base: "300000000", quote: "100000000", usd: "3"},
	}, nine)
	seed(t, db, ctx, ohlcDustPair{base: viaXLM, quote: "native"}, []seedTrade{
		{off: 0, base: "1", quote: "100", usd: ""},
	}, half1)
	if _, err := db.ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	tenth := new(big.Rat).SetFrac64(1, 10)
	want := day.Format("2006-01-02T15:04:05Z")
	check := func(t *testing.T, pts []timescale.AssetPricePoint) {
		t.Helper()
		for _, pt := range pts {
			if pt.T != want {
				continue
			}
			if pt.P == nil {
				t.Fatalf("point %s has no price, want 0.1", want)
			}
			if got, ok := new(big.Rat).SetString(*pt.P); !ok || got.Cmp(tenth) != 0 {
				t.Errorf("point %s = %s, want 0.1 from the 09:00 trade", want, *pt.P)
			}
			return
		}
		t.Fatalf("no point at %s in %d points", want, len(pts))
	}
	for _, asset := range []string{direct, viaXLM} {
		t.Run(asset[:4], func(t *testing.T) {
			pts, err := store.GetAssetPriceHistory7d(ctx, asset)
			if err != nil {
				t.Fatal(err)
			}
			check(t, pts)
			batch, err := store.GetAssetsPriceHistory7dBatch(ctx, []string{asset})
			if err != nil {
				t.Fatal(err)
			}
			check(t, batch[asset])
		})
	}
}

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
	unstamped := "TFRZ-" + zeroLegIssuer
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

	// TFRZ priced three hours ago; its newer XLM trades are unstamped (no fresh XLM/USD anchor), so it has no current USD price.
	seed(t, db, ctx, ohlcDustPair{base: "native", quote: usdcID}, []seedTrade{
		{off: 0, base: "1000000000", quote: "300000000", usd: "30"},
	}, now.Add(-3*time.Hour))
	seed(t, db, ctx, ohlcDustPair{base: unstamped, quote: "native"}, []seedTrade{
		{off: 0, base: "300000000", quote: "100000000", usd: "3"},
	}, now.Add(-3*time.Hour))
	seed(t, db, ctx, ohlcDustPair{base: unstamped, quote: "native"}, []seedTrade{
		{off: 0, base: "300000000", quote: "200000000", usd: ""},
	}, now.Add(-30*time.Minute))

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
	t.Run("unstamped minute is unpriced, not sub-cent", func(t *testing.T) {
		var price string
		err := db.QueryRowContext(ctx, `SELECT price_usd::text FROM asset_price_snapshot WHERE asset_id = $1`, unstamped).Scan(&price)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("price_usd = %s (err %v), want no row: the pre-freeze minute must not be served as current", price, err)
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
