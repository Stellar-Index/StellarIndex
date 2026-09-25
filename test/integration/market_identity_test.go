//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestMarketIdentity_BothOrientationsAreOneMarket executes GH-701's
// readers against one market stored in both directions: sdex writes
// XLM/USDC and USDC/XLM, soroswap only XLM/USDC, aquarius only
// USDC/XLM. Every reader must see ONE market holding all four trades.
// Pre-fix: the asset markets count was 2, top_markets listed USDC twice,
// the per-source asset breakdown gave sdex markets_24h 2, and the pair
// breakdown dropped aquarius entirely and half of sdex.
func TestMarketIdentity_BothOrientationsAreOneMarket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	native := c.NativeAsset()
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := c.NewPair(native, usdc)
	usdcXLM, _ := c.NewPair(usdc, native)

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	for _, tr := range []c.Trade{
		mkIntegrationTrade("sdex", 1, ts, xlmUSDC, 1_000_000_000, 500_000_000),
		mkIntegrationTrade("sdex", 2, ts, usdcXLM, 200_000_000, 400_000_000),
		mkIntegrationTrade("soroswap", 3, ts, xlmUSDC, 600_000_000, 300_000_000),
		mkIntegrationTrade("aquarius", 4, ts, usdcXLM, 100_000_000, 200_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", tr.Source, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// Fixture sanity: both stored orientations really are in prices_1m.
	var dirs int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT (base_asset, quote_asset)) FROM prices_1m
		 WHERE base_asset IN ('native', $1) AND quote_asset IN ('native', $1)`,
		usdc.String()).Scan(&dirs); err != nil {
		t.Fatal(err)
	}
	if dirs != 2 {
		t.Fatalf("fixture: prices_1m holds %d orientations of XLM/USDC, want 2", dirs)
	}

	n, err := store.GetAssetMarketsCount(ctx, native.String())
	if err != nil {
		t.Fatalf("GetAssetMarketsCount: %v", err)
	}
	if n != 1 {
		t.Errorf("GetAssetMarketsCount(native) = %d, want 1 (one market, two stored orientations)", n)
	}

	assertOneTopMarket(t, ctx, store, native.String(), usdc.String())
	assertPairSourceStatsBothOrientations(t, ctx, store, native, usdc)

	assetRows, err := store.AssetSourceStats(ctx, c.AssetAliasStrings(native))
	if err != nil {
		t.Fatalf("AssetSourceStats: %v", err)
	}
	for _, r := range assetRows {
		if r.MarketsCount24h != 1 {
			t.Errorf("AssetSourceStats(native) %s markets_24h = %d, want 1", r.Source, r.MarketsCount24h)
		}
	}

	row, err := store.GetNativeAssetRow(ctx)
	if err != nil {
		t.Fatalf("GetNativeAssetRow: %v", err)
	}
	if !row.ObservationCountUnmeasured {
		t.Errorf("GetNativeAssetRow must flag observation_count unmeasured, got count %d", row.ObservationCount)
	}
}

func assertOneTopMarket(t *testing.T, ctx context.Context, store *timescale.Store, asset, counterparty string) {
	t.Helper()
	tops, err := store.GetAssetTopMarkets(ctx, asset, 5)
	if err != nil {
		t.Fatalf("GetAssetTopMarkets: %v", err)
	}
	if len(tops) != 1 {
		t.Fatalf("GetAssetTopMarkets(%s) = %d markets, want 1: %+v", asset, len(tops), tops)
	}
	m := tops[0]
	if m.Counterparty != counterparty || m.Side != "base" {
		t.Errorf("top market = %+v, want counterparty %s on side base (XLM is the canonical base)", m, counterparty)
	}
	if m.TradeCount24h != 4 {
		t.Errorf("top market trade_count_24h = %d, want 4 (both orientations)", m.TradeCount24h)
	}
}

func assertPairSourceStatsBothOrientations(t *testing.T, ctx context.Context, store *timescale.Store, base, quote c.Asset) {
	t.Helper()
	rows, err := store.PairSourceStats(ctx, c.AssetAliasStrings(base), c.AssetAliasStrings(quote))
	if err != nil {
		t.Fatalf("PairSourceStats: %v", err)
	}
	want := map[string]int64{"sdex": 2, "soroswap": 1, "aquarius": 1}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.Source] = r.TradeCount24h
		if r.MarketsCount24h != 1 {
			t.Errorf("PairSourceStats %s markets_24h = %d, want 1", r.Source, r.MarketsCount24h)
		}
	}
	for src, n := range want {
		if got[src] != n {
			t.Errorf("PairSourceStats %s trades_24h = %d, want %d (all rows: %v)", src, got[src], n, got)
		}
	}
}
