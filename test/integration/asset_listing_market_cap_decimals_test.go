//go:build integration

package integration_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAssetListing_MarketCapUsesTheScaleThePriceIsOn feeds the rollup
// WRITER's output through the one reader that multiplies it.
//
// asset_price_snapshot stores the true-scale price for a confirmed
// non-7-decimals token. The listing's market cap is that price times a
// smallest-unit supply divided by 10^decimals, so the divisor has to be
// the token's real decimals — against the RAW ratio the column used to
// hold, the standard 7 was right only by cancellation. Neither half
// shows the defect alone: the writer's test reads a correct price, and a
// cap test fed a hand-written price proves nothing about what the writer
// stores. This runs the real refresh, the real listing SQL, the real
// supply read and the real handler, and reads the cap off the wire.
//
// Every token circulates exactly 1,000 whole units, at its own scale.
// With the divisor left at 7 the three flagged rows publish 250000.00,
// 1400000000000000.00 and 20.00.
func TestAssetListing_MarketCapUsesTheScaleThePriceIsOn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedDecimalsFixture(t, ctx, store)
	if err := store.RefreshAssetListingRollups(ctx); err != nil {
		t.Fatalf("RefreshAssetListingRollups: %v", err)
	}

	for id, units := range map[string]string{
		decimalsNineContract:     "1000000000000",          // 1,000 x 10^9
		decimalsEighteenContract: "1000000000000000000000", // 1,000 x 10^18
		decimalsFiveContract:     "100000000",              // 1,000 x 10^5
		decimalsSevenContract:    "10000000000",            // 1,000 x 10^7
	} {
		if _, err := store.DB().ExecContext(ctx, `
			INSERT INTO asset_supply_history
			    (time, asset_key, total_supply, circulating_supply, basis, ledger_sequence)
			VALUES (now() - interval '1 minute', $1, $2::numeric, $2::numeric, 'sep41_lake_flows', 50000000)`,
			id, units,
		); err != nil {
			t.Fatalf("seed asset_supply_history %s: %v", id, err)
		}
	}

	// The cache production wires (cmd/stellarindex-api), over the same
	// table the writer joined.
	decimals := v1.NewNonstandardDecimalsCache(store, nil)
	if err := decimals.Refresh(ctx); err != nil {
		t.Fatalf("decimals cache refresh: %v", err)
	}
	srv := v1.New(v1.Options{AssetsReader: store, NonstandardDecimals: decimals})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var env struct {
		Data []v1.AssetDetail `json:"data"`
	}
	getJSON(t, ts.URL+"/v1/assets?type=soroban&limit=50", &env)
	byID := make(map[string]v1.AssetDetail, len(env.Data))
	for _, d := range env.Data {
		byID[d.AssetID] = d
	}

	for id, want := range map[string]struct {
		price, marketCap string
		decimals         int
	}{
		decimalsNineContract:     {"2.5000000000", "2500.00", 9},
		decimalsEighteenContract: {"14.0000000000", "14000.00", 18},
		decimalsFiveContract:     {"2.0000000000", "2000.00", 5},
		decimalsSevenContract:    {"0.6500000000", "650.00", 7}, // unflagged control
	} {
		row, ok := byID[id]
		if !ok {
			t.Errorf("%s: missing from the listing", id)
			continue
		}
		if row.PriceUSD == nil || *row.PriceUSD != want.price {
			t.Errorf("%s: price_usd = %s, want %s", id, derefOrNil(row.PriceUSD), want.price)
		}
		if row.MarketCapUSD == nil || *row.MarketCapUSD != want.marketCap {
			t.Errorf("%s: market_cap_usd = %s, want %s (1,000 tokens x %s)",
				id, derefOrNil(row.MarketCapUSD), want.marketCap, want.price)
		}
		if row.Decimals != want.decimals {
			t.Errorf("%s: decimals = %d, want %d", id, row.Decimals, want.decimals)
		}
	}
}
