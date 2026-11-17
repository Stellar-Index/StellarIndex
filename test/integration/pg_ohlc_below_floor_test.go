//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TradesInRange carries the stored bars' per-trade floor: a $1000 trade clears it, a sub-cent one does not, and one
// whose usd_volume is still NULL does not either, in both stored directions.
func TestTradesInRangeClearsNotionalFloor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	base, err := canonical.NewClassicAsset("TRFL", issuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(base, canonical.NativeAsset())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	seed(t, store.DB(), ctx, ohlcDustPair{base: "TRFL-" + issuer, quote: "native"}, []seedTrade{
		{off: 0, base: "10000000", quote: "54875000", usd: "1000"},
		{off: time.Second, base: "2", quote: "15", usd: "0.00000027"},
		{off: 2 * time.Second, base: "10000000", quote: "54875000"},
	}, t0)
	seed(t, store.DB(), ctx, ohlcDustPair{base: "native", quote: "TRFL-" + issuer}, []seedTrade{
		{off: 3 * time.Second, base: "54875000", quote: "10000000", usd: "0.01"},
	}, t0)

	trades, err := store.TradesInRange(ctx, pair, t0, t0.Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	want := []bool{true, false, false, true}
	if len(trades) != len(want) {
		t.Fatalf("got %d trades, want %d", len(trades), len(want))
	}
	for i, w := range want {
		if trades[i].ClearsNotionalFloor != w {
			t.Errorf("trade %d (+%s): ClearsNotionalFloor = %v, want %v", i, trades[i].Timestamp.Sub(t0), trades[i].ClearsNotionalFloor, w)
		}
	}
}

// A folded bar is below the floor only when none of its source minutes cleared it: dust and unstamped (NULL usd_volume) minutes
// alone set the bit, as they leave notional_trade_count at 0, and one cleared minute among them clears it.
func TestOHLCSeriesReBucketed_BelowFloorAcrossMinutes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	base, err := canonical.NewClassicAsset("BLWM", issuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(base, canonical.NativeAsset())
	if err != nil {
		t.Fatal(err)
	}

	const fold = 30 * time.Minute
	t0 := timeBucket(fold, time.Now().UTC().Add(-30*time.Hour))
	p := ohlcDustPair{base: "BLWM-" + issuer, quote: "native"}
	seed(t, store.DB(), ctx, p, []seedTrade{
		{off: time.Minute, base: "2", quote: "15", usd: "0.00000027"},
		{off: 5 * time.Minute, base: "10000000", quote: "54875000"},
		{off: 9 * time.Minute, base: "10000000", quote: "10000", usd: "0.000001"},
	}, t0)
	seed(t, store.DB(), ctx, p, []seedTrade{
		{off: time.Minute, base: "2", quote: "15", usd: "0.00000027"},
		{off: 7 * time.Minute, base: "10000000", quote: "54875000", usd: "1000"},
		{off: 12 * time.Minute, base: "10000000", quote: "10000", usd: "0.000001"},
	}, t0.Add(fold))
	refreshPriceCAGGs(t, store.DB(), ctx)

	bars, err := store.OHLCSeriesReBucketed(ctx, pair, timescale.Granularity1m, "30 minutes", t0, t0.Add(2*fold), 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("got %d bars, want 2", len(bars))
	}
	if !bars[0].BelowFloor || bars[0].TradeCount != 3 {
		t.Errorf("dust + unstamped bar at %s: BelowFloor=%v trades=%d, want true over 3 trades", bars[0].Bucket, bars[0].BelowFloor, bars[0].TradeCount)
	}
	if bars[1].BelowFloor || bars[1].TradeCount != 3 {
		t.Errorf("bar with one cleared minute at %s: BelowFloor=%v trades=%d, want false over 3 trades", bars[1].Bucket, bars[1].BelowFloor, bars[1].TradeCount)
	}
}

// Both OHLC readers' below_floor bit on real CAGG rows: a bucket whose every trade is under the $0.01 notional floor
// is below the floor, and a bucket with one trade clearing it is not, even beside dust. 1m reads notional_trade_count,
// coarser grains read volume_usd, so both predicates are exercised.
func TestOHLCSeriesBelowFloorBit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	base, err := canonical.NewClassicAsset("BLWF", issuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(base, canonical.NativeAsset())
	if err != nil {
		t.Fatal(err)
	}

	const fold = 4 * time.Hour
	t0 := timeBucket(fold, time.Now().UTC().Add(-30*time.Hour))
	dustAt, flooredAt := t0.Add(10*time.Minute), t0.Add(fold+10*time.Minute)
	p := ohlcDustPair{base: "BLWF-" + issuer, quote: "native"}
	seed(t, store.DB(), ctx, p, []seedTrade{
		{off: 0, base: "2", quote: "15", usd: "0.00000027"},
		{off: 30 * time.Second, base: "10000000", quote: "10000", usd: "0.000001"},
	}, dustAt)
	seed(t, store.DB(), ctx, p, []seedTrade{
		{off: 0, base: "2", quote: "15", usd: "0.00000027"},
		{off: 30 * time.Second, base: "10000000", quote: "54875000", usd: "1000"},
	}, flooredAt)
	refreshPriceCAGGs(t, store.DB(), ctx)

	from, to := t0, t0.Add(2*fold)
	reads := map[string]func() ([]timescale.OHLCBar, error){
		"OHLCSeries/1m": func() ([]timescale.OHLCBar, error) {
			return store.OHLCSeries(ctx, pair, timescale.Granularity1m, from, to, 0)
		},
		"OHLCSeries/1h": func() ([]timescale.OHLCBar, error) {
			return store.OHLCSeries(ctx, pair, timescale.Granularity1h, from, to, 0)
		},
		"OHLCSeriesReBucketed/1h->4h": func() ([]timescale.OHLCBar, error) {
			return store.OHLCSeriesReBucketed(ctx, pair, timescale.Granularity1h, "4 hours", from, to, 0)
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			bars, err := read()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(bars) != 2 {
				t.Fatalf("got %d bars, want 2 (one dust-only, one above the floor)", len(bars))
			}
			if !bars[0].BelowFloor {
				t.Errorf("dust-only bar at %s: BelowFloor = false, want true", bars[0].Bucket)
			}
			if bars[1].BelowFloor {
				t.Errorf("bar with a $1000 trade at %s: BelowFloor = true, want false", bars[1].Bucket)
			}
		})
	}
}
