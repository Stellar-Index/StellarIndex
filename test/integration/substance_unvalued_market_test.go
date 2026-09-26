//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSubstanceGate_TellsAnUnvaluableMarketFromAThinOne executes both
// substance reads against real TimescaleDB and runs the gate over them
// (GH-1052). prices_1m stores sum(coalesce(usd_volume, 0)), so a market
// the insert-time waterfall could not value reads as $0 — the same
// number as a market that was valued and found empty. The reads now
// return how many active buckets carried a dollar value, and the gate
// names the two failures apart.
//
//   - UNVA/PAIR — 30 minutes of trades over 9h40m with usd_volume NULL
//     (the SEP-41/SEP-41 shape): clears buckets and span, $0 volume.
//   - THIN/native — the same activity, every trade valued at $1: a
//     genuinely thin $30 market.
//
// Both stay withheld; only the reason differs.
func TestSubstanceGate_TellsAnUnvaluableMarketFromAThinOne(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	unva, err := c.NewClassicAsset("UNVA", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pairLeg, err := c.NewClassicAsset("PAIR", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	thin, err := c.NewClassicAsset("THIN", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	unvaluedPair, _ := c.NewPair(unva, pairLeg)
	thinPair, _ := c.NewPair(thin, c.NativeAsset())

	now := time.Now().UTC().Truncate(time.Minute)
	nonce := 0
	for i := 0; i < 30; i++ {
		ts := now.Add(-10*time.Hour + time.Duration(i)*20*time.Minute)
		for _, pair := range []c.Pair{unvaluedPair, thinPair} {
			nonce++
			if err := store.InsertTrade(ctx, mkAPITrade(nonce, ts, pair, 1_000_000, 500_000)); err != nil {
				t.Fatalf("InsertTrade: %v", err)
			}
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = NULL WHERE source = 'integ-api' AND base_asset = $1`, unva.String()); err != nil {
		t.Fatalf("clear usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 1 WHERE source = 'integ-api' AND base_asset = $1`, thin.String()); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	const (
		wantBuckets = 30
		wantSpan    = 29 * 20 * 60
	)
	check := func(name string, got timescale.MarketSubstance, volume float64, valued int64) {
		t.Helper()
		if v := mustFloat(t, got.VolumeUSD); v != volume || got.Buckets != wantBuckets ||
			got.SpanSeconds != wantSpan || got.ValuedBuckets != valued {
			t.Errorf("%s: got {volume %s, buckets %d, span %ds, valued %d}, want {volume %v, buckets %d, span %ds, valued %d}",
				name, got.VolumeUSD, got.Buckets, got.SpanSeconds, got.ValuedBuckets,
				volume, wantBuckets, wantSpan, valued)
		}
	}
	day := 24 * time.Hour
	for _, tc := range []struct {
		pair   c.Pair
		volume float64
		valued int64
	}{
		{unvaluedPair, 0, 0},
		{thinPair, 30, wantBuckets},
	} {
		bases, quotes := c.AssetAliases(tc.pair.Base), c.AssetAliases(tc.pair.Quote)
		live, err := store.PairMarketSubstance(ctx, bases, quotes, day)
		if err != nil {
			t.Fatalf("PairMarketSubstance(%s): %v", tc.pair, err)
		}
		check(tc.pair.String()+" trailing", live, tc.volume, tc.valued)
		at, err := store.PairMarketSubstanceAt(ctx, bases, quotes, now, day, timescale.Granularity1m)
		if err != nil {
			t.Fatalf("PairMarketSubstanceAt(%s): %v", tc.pair, err)
		}
		check(tc.pair.String()+" at now", at, tc.volume, tc.valued)
	}

	gate := pricingguard.NewSubstanceGate(store, pricingguard.SubstanceGateOptions{})
	for _, tc := range []struct {
		base, quote c.Asset
		want        pricingguard.SubstanceFloor
	}{
		{unva, pairLeg, pricingguard.FloorVolumeUnvalued},
		{thin, c.NativeAsset(), pricingguard.FloorVolume},
	} {
		allowed, measured, floor := gate.Probe(ctx, tc.base, tc.quote)
		if allowed || !measured || floor != tc.want {
			t.Errorf("Probe(%s, %s) = (allowed %v, measured %v, floor %q), want (false, true, %q)",
				tc.base, tc.quote, allowed, measured, floor, tc.want)
		}
	}
}
