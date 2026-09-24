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

// TestSubstanceGate_AliasUnionCountsSharedMinutesOnce executes the
// substance gate's alias union on real prices_1m rows. XLM's SDEX leg
// (native) and CEX leg (crypto:XLM) trading in the SAME ten minutes are
// ten distinct minutes of market; counting each spelling's minutes and
// adding them read twenty and cleared the default 20-minute floor.
func TestSubstanceGate_AliasUnionCountsSharedMinutesOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cexXLM, err := c.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := c.NewClassicAsset("SHRD", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	disjoint, err := c.NewClassicAsset("DSJT", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}

	// Ten minutes spread over 7h03m, well inside the trailing day.
	start := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Hour)
	nonce := 0
	add := func(ts time.Time, base, quote c.Asset) {
		t.Helper()
		pair, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		nonce++
		if err := store.InsertTrade(ctx, mkAPITrade(nonce, ts.Add(10*time.Second), pair, 1_000_000, 500_000)); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	for i := 0; i < 10; i++ {
		minute := start.Add(time.Duration(i) * 47 * time.Minute)
		// SHRD: both spellings in the same minute.
		add(minute, c.NativeAsset(), shared)
		add(minute, cexXLM, shared)
		// DSJT, the control: the same volume in ten DIFFERENT minutes
		// per spelling — twenty distinct minutes of real market.
		add(minute, c.NativeAsset(), disjoint)
		add(minute.Add(20*time.Minute), cexXLM, disjoint)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 500 WHERE source = 'integ-api'`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	sub, err := store.PairMarketSubstance(ctx, c.AssetAliases(c.NativeAsset()), c.AssetAliases(shared), 24*time.Hour)
	if err != nil {
		t.Fatalf("PairMarketSubstance: %v", err)
	}
	if mustFloat(t, sub.VolumeUSD) != 10_000 || sub.Buckets != 10 || sub.SpanSeconds != 9*47*60 {
		t.Errorf("SHRD union = {volume %s, buckets %d, span %ds}, want {10000, 10, %ds}: "+
			"volumes add across spellings, a shared minute is one bucket",
			sub.VolumeUSD, sub.Buckets, sub.SpanSeconds, 9*47*60)
	}

	gate := pricingguard.NewSubstanceGate(store, pricingguard.SubstanceGateOptions{})
	allowed, measured := gate.Verdict(ctx, c.NativeAsset(), shared, "test")
	if !measured {
		t.Fatal("SHRD: verdict unmeasured")
	}
	if allowed {
		t.Error("SHRD cleared the 20-distinct-minute floor on 10 minutes quoted under two XLM spellings")
	}
	allowed, measured = gate.Verdict(ctx, c.NativeAsset(), disjoint, "test")
	if !measured || !allowed {
		t.Errorf("DSJT (control, 20 distinct minutes over 7h): allowed=%v measured=%v, want served", allowed, measured)
	}
}
