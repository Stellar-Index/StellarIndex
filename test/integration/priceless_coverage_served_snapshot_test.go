//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPricelessCoverage_ServedSnapshotCountsAsPriced: an asset with priced
// 7d volume but no proxy market above the substance floors is a candidate
// until the listing serves it a price. A fresh asset_price_snapshot row
// takes it out of the candidate set and makes AssetIsPriced true; a row
// past the listing's staleness bound does neither.
func TestPricelessCoverage_ServedSnapshotCountsAsPriced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	asset, err := c.NewClassicAsset("SERVEDQ", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(asset, usd)
	if err != nil {
		t.Fatal(err)
	}

	// One thin trade: priced 7d volume, far under every substance floor.
	tr := mkIntegrationTrade("sdex", 1, time.Now().UTC().Truncate(time.Minute).Add(-2*time.Hour), pair, 1_000_000_000, 1_000_000_000)
	if err := store.InsertTrade(ctx, tr); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 50 WHERE base_asset = $1`, asset.String()); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	check := func(label string, wantPriced bool) {
		t.Helper()
		priced, err := store.AssetIsPriced(ctx, asset.String())
		if err != nil {
			t.Fatalf("%s: AssetIsPriced: %v", label, err)
		}
		if priced != wantPriced {
			t.Errorf("%s: AssetIsPriced = %v, want %v", label, priced, wantPriced)
		}
		sigs, err := store.PopularPricelessCandidates(ctx)
		if err != nil {
			t.Fatalf("%s: PopularPricelessCandidates: %v", label, err)
		}
		candidate := false
		for _, s := range sigs {
			if s.AssetID == asset.String() {
				candidate = true
			}
		}
		if candidate == wantPriced {
			t.Errorf("%s: candidate = %v, want %v", label, candidate, !wantPriced)
		}
	}

	check("no snapshot row", false)

	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO asset_price_snapshot (asset_id, price_usd, source_count, computed_at)
		 VALUES ($1, 0.000002877964831796519168, 1, now() - INTERVAL '1 hour')`, asset.String()); err != nil {
		t.Fatalf("insert stale snapshot: %v", err)
	}
	check("stale snapshot row", false)

	if _, err := store.DB().ExecContext(ctx,
		`UPDATE asset_price_snapshot SET computed_at = now() WHERE asset_id = $1`, asset.String()); err != nil {
		t.Fatalf("freshen snapshot: %v", err)
	}
	check("fresh snapshot row", true)
}
