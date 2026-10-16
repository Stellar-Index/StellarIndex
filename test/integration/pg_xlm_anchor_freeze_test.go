//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestXLMUSDAnchor_FrozenMinuteAnchorsNothing: an XLM/USD-proxy minute inside a recorded freeze (ADR-0019) prices neither an
// XLM-quoted token's sparkline point nor the live XLM price; minutes outside it still do.
func TestXLMUSDAnchor_FrozenMinuteAnchorsNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c.InstallAliasRegistry(nil)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	spec, err := timescale.NewUSDVolumeQuoteSpec([]string{"USDC-" + usdcIssuer}, nil)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)
	usdc, err := c.NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	tkn, err := c.NewClassicAsset("TKN", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	nativeUSDC, _ := c.NewPair(c.NativeAsset(), usdc)
	tknXLM, _ := c.NewPair(tkn, c.NativeAsset())

	now := time.Now().UTC()
	fresh := now.Add(-10 * time.Minute).Truncate(time.Minute)
	frozen := now.Add(-2 * time.Hour).Truncate(time.Minute)
	clear := now.Add(-5 * time.Hour).Truncate(time.Minute)
	for i, tr := range []c.Trade{
		mkIntegrationTrade("soroswap", 1, clear, nativeUSDC, 1_000_000_000, 300_000_000),  // 0.3
		mkIntegrationTrade("soroswap", 2, frozen, nativeUSDC, 1_000_000_000, 500_000_000), // 0.5
		mkIntegrationTrade("soroswap", 3, fresh, nativeUSDC, 1_000_000_000, 600_000_000),  // 0.6
		mkIntegrationTrade("soroswap", 4, clear, tknXLM, 1_000_000_000, 2_000_000_000),    // 2 XLM
		mkIntegrationTrade("soroswap", 5, frozen, tknXLM, 1_000_000_000, 2_000_000_000),   // 2 XLM
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	read := func() ([]timescale.AssetPricePoint, *string) {
		t.Helper()
		pts, err := store.GetAssetPriceHistory24h(ctx, tkn.String())
		if err != nil {
			t.Fatalf("GetAssetPriceHistory24h: %v", err)
		}
		row, err := store.GetNativeAssetRow(ctx)
		if err != nil {
			t.Fatalf("GetNativeAssetRow: %v", err)
		}
		return pts, row.PriceUSD
	}

	// Control: with no freeze every minute anchors.
	pts, price := read()
	assertPointAt(t, "unfrozen", pts, frozen.Truncate(time.Hour), 1.0)
	assertPointAt(t, "unfrozen", pts, clear.Truncate(time.Hour), 0.6)
	if price == nil || mustFloat(t, *price) < 0.599 || mustFloat(t, *price) > 0.601 {
		t.Fatalf("unfrozen GetNativeAssetRow.PriceUSD = %s, want 0.6", deref(price))
	}

	// One recovered freeze over the 2 h minute, one still open over the fresh one.
	for _, f := range []struct {
		at        time.Time
		recovered any
	}{{frozen.Add(-time.Minute), frozen.Add(30 * time.Minute)}, {fresh.Add(-time.Minute), nil}} {
		if _, err := store.DB().ExecContext(ctx, `
			INSERT INTO freeze_events (asset_id, quote_id, frozen_at, frozen_at_ledger, reason, frozen_value, recovered_at)
			VALUES ('native', $1, $2, 1, 'manual', 0.5, $3)`, usdc.String(), f.at, f.recovered); err != nil {
			t.Fatalf("seed freeze at %s: %v", f.at, err)
		}
	}

	pts, price = read()
	key := frozen.Truncate(time.Hour).Format("2006-01-02T15:04:05Z")
	for _, p := range pts {
		if p.T == key && p.P != nil {
			t.Errorf("TKN 24h point %s = %s, want null: the hour's only XLM/USD minute is frozen", key, *p.P)
		}
	}
	assertPointAt(t, "frozen", pts, clear.Truncate(time.Hour), 0.6)
	if price != nil {
		t.Errorf("GetNativeAssetRow.PriceUSD = %s, want unpriced: the only fresh XLM/USD minute is frozen", *price)
	}
}
