//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPricelessCoverage_PricedDirectAppliesSubstanceFloors is a
// regression guard: priced_direct must not count an asset as priced on a
// SINGLE unqualified prices_1m row, while its sibling one_hop CTE applies
// three substance floors (vol_usd >= 1000, buckets >= 20, span_s >=
// 21600) and says in its own comment that they are "NOT decoration". The
// two arms of "is this asset priced?" must agree.
//
// THIN: one $50 trade against a USD proxy — a single bucket, single
// minute of span, well under every floor. Must NOT be priced.
//
// SUBSTANTIAL: 21 trades against the same proxy, 20 minutes apart,
// spanning ~6h40m and summing to $2,100 — clears all three floors. Must
// be priced, proving the added floors do not regress a real market.
func TestPricelessCoverage_PricedDirectAppliesSubstanceFloors(t *testing.T) {
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
	thin, err := c.NewClassicAsset("THINQ", issuer)
	if err != nil {
		t.Fatal(err)
	}
	solid, err := c.NewClassicAsset("SOLIDQ", issuer)
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	thinPair, err := c.NewPair(thin, usd)
	if err != nil {
		t.Fatal(err)
	}
	solidPair, err := c.NewPair(solid, usd)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Hour)
	nonce := 0

	// THIN: one trade, one bucket.
	nonce++
	tr := mkIntegrationTrade("sdex", nonce, now, thinPair, 1_000_000_000, 1_000_000_000)
	if err := store.InsertTrade(ctx, tr); err != nil {
		t.Fatalf("InsertTrade thin: %v", err)
	}

	// SOLID: 21 trades, 20 minutes apart -> 21 distinct 1-minute buckets,
	// ~400 minutes (6h40m) of span, comfortably clearing every floor.
	solidStart := now.Add(-7 * time.Hour)
	for i := 0; i < 21; i++ {
		nonce++
		ts := solidStart.Add(time.Duration(i) * 20 * time.Minute)
		tr := mkIntegrationTrade("sdex", nonce, ts, solidPair, 1_000_000_000, 1_000_000_000)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade solid %d: %v", i, err)
		}
	}

	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 50 WHERE base_asset = $1`, thin.String()); err != nil {
		t.Fatalf("stamp thin usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 100 WHERE base_asset = $1`, solid.String()); err != nil {
		t.Fatalf("stamp solid usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	priced, err := store.AssetIsPriced(ctx, thin.String())
	if err != nil {
		t.Fatalf("AssetIsPriced thin: %v", err)
	}
	if priced {
		t.Errorf("thin (single row, $50, one bucket) reads priced=true, want false — " +
			"priced_direct must apply the same substance floors as one_hop")
	}

	priced, err = store.AssetIsPriced(ctx, solid.String())
	if err != nil {
		t.Fatalf("AssetIsPriced solid: %v", err)
	}
	if !priced {
		t.Errorf("solid ($2,100 over 21 buckets, ~6h40m span) reads priced=false, want true — " +
			"a genuinely substantial direct market must still be priced")
	}
}
