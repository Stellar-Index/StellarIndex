//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestVWAPUSDFXResolver_SubstanceGate executes the valuation substance
// gate against real TimescaleDB, with the exact tiers installed so peg
// markets carry the volume_usd production gives them.
//
//   - USDX: the wash-ring shape. 0.1 USDX <-> 0.1 USDC round trips every
//     20 min ($4.80/day) set a $1 rate, and a two-trade USDX/XLM book
//     sets the same $1 through the bridge. Without the gate the resolver
//     prices USDX at $1; with it, neither market clears the floor.
//   - DEEP: 30 half-hourly $108.50 trades against USDC — clears the floor.
//   - OLD: the same market 30 days ago and nothing since — it values a
//     trade at its own time, held to the hour-grain floor.
func TestVWAPUSDFXResolver_SubstanceGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		usdcIssuer  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		tokenIssuer = "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	)
	usdcID := "USDC-" + usdcIssuer
	if err := timescale.InstallUSDVolumeResolution(store, []string{usdcID}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}
	asset := func(code, issuer string) c.Asset {
		a, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	pair := func(base, quote c.Asset) c.Pair {
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	usdc := asset("USDC", usdcIssuer)
	usdx, deep, old := asset("USDX", tokenIssuer), asset("DEEP", tokenIssuer), asset("OLD", tokenIssuer)

	at := time.Now().UTC().Truncate(time.Minute)
	hour := at.Truncate(time.Hour)
	then := hour.Add(-30 * 24 * time.Hour)

	nonce := 0
	insert := func(ts time.Time, p c.Pair, base, quote int64) {
		t.Helper()
		nonce++
		if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", nonce, ts, p, base, quote)); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	for i := range 30 {
		step := time.Duration(i) * 30 * time.Minute
		insert(hour.Add(-15*time.Hour+step), pair(c.NativeAsset(), usdc), 10_000_000_000, 2_500_000_000) // XLM at $0.25
		insert(hour.Add(-15*time.Hour+step), pair(deep, usdc), 1_000_000_000, 1_085_000_000)
		insert(then.Add(-15*time.Hour+step), pair(old, usdc), 1_000_000_000, 1_085_000_000)
	}
	for i := range 48 {
		insert(hour.Add(-17*time.Hour+time.Duration(i)*20*time.Minute), pair(usdx, usdc), 1_000_000, 1_000_000)
	}
	insert(hour.Add(-3*time.Hour), pair(usdx, c.NativeAsset()), 100_000_000, 400_000_000)
	insert(hour.Add(-2*time.Hour), pair(usdx, c.NativeAsset()), 100_000_000, 400_000_000)
	// The XLM-quote anchor read prices_1m before it was refreshed; stamp the
	// ring's XLM leg at its true $10 so the bridge has a bucket to use.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 10 WHERE base_asset = $1 AND quote_asset = 'native'`, usdx.String()); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	for _, view := range []string{"prices_1m", "prices_1h"} {
		if _, err := store.DB().ExecContext(ctx,
			`CALL refresh_continuous_aggregate('`+view+`', NULL, NULL)`); err != nil {
			t.Fatalf("refresh %s: %v", view, err)
		}
	}

	resolver := func(disableGate bool) *timescale.VWAPUSDFXResolver {
		t.Helper()
		r, err := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
			USDPegs:              []string{usdcID},
			Freshness:            -1, // staleness is not under test
			DisableSubstanceGate: disableGate,
		})
		if err != nil {
			t.Fatalf("NewVWAPUSDFXResolver: %v", err)
		}
		return r
	}
	price := func(r *timescale.VWAPUSDFXResolver, a c.Asset, ts time.Time) (string, bool) {
		t.Helper()
		got, ok, err := r.USDPriceAt(ctx, a, ts)
		if err != nil {
			t.Fatalf("USDPriceAt(%s): %v", a, err)
		}
		return trimTrailingZeros(got), ok
	}

	if got, ok := price(resolver(true), usdx, at); !ok || got != "1" {
		t.Fatalf("fixture: ungated USDPriceAt(USDX) = (%q, %t), want the ring's $1", got, ok)
	}
	gated := resolver(false)
	if got, ok := price(gated, usdx, at); ok {
		t.Errorf("USDPriceAt(USDX) = %q: a wash-ring market valued a trade", got)
	}
	if got, ok := price(gated, deep, at); !ok || got != "1.085" {
		t.Errorf("USDPriceAt(DEEP) = (%q, %t), want 1.085 from a market with substance", got, ok)
	}
	if got, ok := price(gated, old, then); !ok || got != "1.085" {
		t.Errorf("USDPriceAt(OLD, 30d ago) = (%q, %t), want 1.085: deep then, dormant now", got, ok)
	}
	if got, ok := price(gated, c.NativeAsset(), at); !ok || got != "0.25" {
		t.Errorf("USDPriceAt(XLM) = (%q, %t), want the ungated 0.25 anchor", got, ok)
	}
}
