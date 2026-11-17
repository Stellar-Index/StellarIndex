//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

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
