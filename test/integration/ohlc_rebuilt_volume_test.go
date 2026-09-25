//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestOHLCRebuiltVolumeIsTheIntegerSum executes both OHLC readers over a
// bucket whose volume legs have to be rebuilt from vwap·volume in both
// stored directions, with a vwap (1/3) that NUMERIC division cannot
// represent exactly. PostgreSQL stores 1e6/3e6 as 0.33333333333333333333,
// so the bare product serves 999999.99999999999999000000 where the trades
// sum to exactly 1000000. Volumes are integer smallest-unit sums; the
// served value must be the integer.
func TestOHLCRebuiltVolumeIsTheIntegerSum(t *testing.T) {
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
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	flipped, err := c.NewPair(usdc, c.NativeAsset())
	if err != nil {
		t.Fatal(err)
	}

	const fold = 4 * time.Hour
	t0 := timeBucket(fold, time.Now().UTC().Add(-30*time.Hour))
	// Requested direction: 3e6 XLM for 1e6 USDC (quote leg rebuilt).
	// Flipped direction: 3e6 USDC for 1e6 XLM (base leg rebuilt).
	// Either way the served bar is 4e6 XLM against 4e6 USDC.
	for i, tr := range []c.Trade{
		mkAPITrade(1, t0.Add(10*time.Minute), pair, 3_000_000, 1_000_000),
		mkAPITrade(2, t0.Add(20*time.Minute), flipped, 3_000_000, 1_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1h', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1h: %v", err)
	}

	native, err := store.OHLCSeries(ctx, pair, timescale.Granularity1h, t0, t0.Add(fold), 10)
	if err != nil {
		t.Fatalf("OHLCSeries: %v", err)
	}
	folded, err := store.OHLCSeriesReBucketed(ctx, pair, timescale.Granularity1h, "4 hours", t0, t0.Add(fold), 10)
	if err != nil {
		t.Fatalf("OHLCSeriesReBucketed: %v", err)
	}
	for name, bars := range map[string][]timescale.OHLCBar{"OHLCSeries": native, "OHLCSeriesReBucketed": folded} {
		if len(bars) != 1 {
			t.Fatalf("%s returned %d bars, want 1", name, len(bars))
		}
		if got := bars[0].BaseVolume; got != "4000000" {
			t.Errorf("%s base_volume = %q, want exactly 4000000 (3e6 stored + 1e6 rebuilt from the flipped row)", name, got)
		}
		if got := bars[0].QuoteVolume; got != "4000000" {
			t.Errorf("%s quote_volume = %q, want exactly 4000000 (1e6 rebuilt + 3e6 stored on the flipped row)", name, got)
		}
	}
}
