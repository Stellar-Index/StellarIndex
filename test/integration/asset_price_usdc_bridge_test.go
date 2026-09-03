//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAssetPriceUSDCQuotedOnly proves the stablecoin-proxy + SAC-alias
// bridges in every asset_catalogue price path, via three synthetic
// assets that each have EXACTLY ONE kind of market:
//
//   - ZAUD: quoted only in classic USDC (GA5Z…)  → direct USD price
//   - ZUSC: quoted only in the USDC SAC (CCW67T…) → direct USD price
//   - ZSAC: quoted only in the XLM SAC (CAS3J7…)  → vwap × xlm_usd
//
// None has a fiat:USD or plain-'native' pair, so before the fix every
// one of them priced NULL: the direct_usd* CTEs accepted only
// quote_asset = 'fiat:USD' and the asset_vs_xlm* CTEs only 'native'.
// Regression guard for the 2026-08-24 operator report (473/500
// /v1/assets rows priceless — AUDD with $344k 24h volume, EURC, the
// *allow variants, and the Soroban-venue majority quoted in SAC ids).
func TestAssetPriceUSDCQuotedOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Circle's canonical USDC issuer + the two well-known SAC
	// contract ids — must match the literals in asset_catalogue.go's
	// stablecoin-proxy / XLM-leg IN lists (XLM SAC =
	// canonical.XLMSacContractID).
	const (
		usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		usdcSAC    = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
		xlmSAC     = c.XLMSacContractID
	)
	// Real CRC-valid issuer strkeys (AQUA's + the issuers-test third
	// issuer) so canonical.NewClassicAsset's checksum passes.
	const (
		zaudIssuer = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		zsacIssuer = "GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M"
	)

	mustClassic := func(code, issuer string) c.Asset {
		t.Helper()
		a, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	mustSoroban := func(contractID string) c.Asset {
		t.Helper()
		a, err := c.NewSorobanAsset(contractID)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	mustPair := func(base, quote c.Asset) c.Pair {
		t.Helper()
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	usdc := mustClassic("USDC", usdcIssuer)
	zaud := mustClassic("ZAUD", zaudIssuer)
	zusc := mustClassic("ZUSC", usdcIssuer)
	zsac := mustClassic("ZSAC", zsacIssuer)

	zaudID, zuscID, zsacID := zaud.String(), zusc.String(), zsac.String()

	seedIssuers(t, ctx, store, []seedIssuer{
		{g: zaudIssuer, homeDomain: ""},
		{g: zsacIssuer, homeDomain: ""},
		{g: usdcIssuer, homeDomain: ""},
	})
	seedClassicAssets(t, ctx, store, []seedAsset{
		{assetID: zaudID, code: "ZAUD", issuer: zaudIssuer, slug: "ZAUD", obs: 3_000},
		{assetID: zuscID, code: "ZUSC", issuer: usdcIssuer, slug: "ZUSC", obs: 2_000},
		{assetID: zsacID, code: "ZSAC", issuer: zsacIssuer, slug: "ZSAC", obs: 1_000},
	})

	// Two trades per pair, 20 min apart, all inside every query's
	// "now" window. The freshest bucket must win each direct pick.
	// XLM/USDC (classic) at a constant 0.40 seeds the xlm_usd CTEs so
	// the ZSAC triangulation has a USD leg — NO plain-'native' and NO
	// fiat:USD rows exist for any of the three assets under test.
	now := time.Now().UTC().Truncate(time.Minute)
	early, late := now.Add(-30*time.Minute), now.Add(-10*time.Minute)
	trades := []c.Trade{
		// ZAUD/USDC-classic: 0.60 → 0.65.
		mkIntegrationTrade("sdex", 1, early, mustPair(zaud, usdc), 1_000_000_000, 600_000_000),
		mkIntegrationTrade("sdex", 2, late, mustPair(zaud, usdc), 1_000_000_000, 650_000_000),
		// ZUSC/USDC-SAC: 0.98 → 0.99.
		mkIntegrationTrade("soroswap", 3, early, mustPair(zusc, mustSoroban(usdcSAC)), 1_000_000_000, 980_000_000),
		mkIntegrationTrade("soroswap", 4, late, mustPair(zusc, mustSoroban(usdcSAC)), 1_000_000_000, 990_000_000),
		// ZSAC/XLM-SAC: 2.0 → 1.5 (in XLM).
		mkIntegrationTrade("soroswap", 5, early, mustPair(zsac, mustSoroban(xlmSAC)), 1_000_000_000, 2_000_000_000),
		mkIntegrationTrade("soroswap", 6, late, mustPair(zsac, mustSoroban(xlmSAC)), 1_000_000_000, 1_500_000_000),
		// XLM/USDC-classic: constant 0.40 (the xlm_usd USD leg).
		mkIntegrationTrade("sdex", 7, early, mustPair(c.NativeAsset(), usdc), 1_000_000_000, 400_000_000),
		mkIntegrationTrade("sdex", 8, late, mustPair(c.NativeAsset(), usdc), 1_000_000_000, 400_000_000),
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	// Force the cagg refresh — the 30s policy won't fire inside the
	// test window (mirrors trades_range_test.go).
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	// The LISTING's price column now comes from asset_price_snapshot
	// (migration 0154, #331 F1) rather than from twelve per-request
	// prices_1m CTEs, so refreshing the cagg alone leaves the listing
	// arm of this test unpriced. The single-asset arm still reads
	// prices_1m live and is unaffected — which is exactly the split
	// this refresh keeps honest.
	if err := store.RefreshAssetListingRollups(ctx); err != nil {
		t.Fatalf("RefreshAssetListingRollups: %v", err)
	}

	// Expected price_usd per asset: {latest, earlier} — trades may
	// straddle an hour/day boundary depending on when the test runs,
	// so series checks accept either seeded print but require the
	// LATEST non-null point to be the fresh one.
	cases := []struct {
		name, assetID, slug, wantLatest, wantEarly string
	}{
		{"classic-USDC quote", zaudID, "ZAUD", "0.6500000000", "0.6000000000"},
		{"USDC-SAC quote", zuscID, "ZUSC", "0.9900000000", "0.9800000000"},
		{"XLM-SAC quote triangulated", zsacID, "ZSAC", "0.6000000000", "0.8000000000"}, // 1.5×0.40, 2.0×0.40
	}

	// Errorf (not Fatalf) so each of the three quote-form cases
	// reports independently — a regression in one bridge must not
	// mask the others.
	checkPrice := func(t *testing.T, label string, got *string, want string) {
		t.Helper()
		if got == nil {
			t.Errorf("%s price_usd = nil, want %s", label, want)
			return
		}
		if *got != want {
			t.Errorf("%s price_usd = %s, want %s", label, *got, want)
		}
	}
	checkSeries := func(t *testing.T, label string, pts []timescale.AssetPricePoint, wantLatest, wantEarly string) {
		t.Helper()
		var lastNonNil string
		for _, pt := range pts {
			if pt.P == nil {
				continue
			}
			if *pt.P != wantLatest && *pt.P != wantEarly {
				t.Errorf("%s: unexpected series price %s at %s", label, *pt.P, pt.T)
			}
			lastNonNil = *pt.P
		}
		if lastNonNil == "" {
			t.Errorf("%s: series has no non-null points, want latest %s", label, wantLatest)
			return
		}
		if lastNonNil != wantLatest {
			t.Errorf("%s: latest series price = %s, want %s", label, lastNonNil, wantLatest)
		}
	}

	t.Run("ListAssets", func(t *testing.T) {
		got, err := store.ListAssets(ctx, 10, "", "")
		if err != nil {
			t.Fatalf("ListAssets: %v", err)
		}
		byID := make(map[string]timescale.AssetRow, len(got))
		for _, r := range got {
			byID[r.AssetID] = r
		}
		for _, tc := range cases {
			row, ok := byID[tc.assetID]
			if !ok {
				t.Fatalf("%s: row %s missing from listing", tc.name, tc.assetID)
			}
			checkPrice(t, "listing "+tc.name, row.PriceUSD, tc.wantLatest)
		}
	})

	t.Run("GetAssetBySlug", func(t *testing.T) {
		for _, tc := range cases {
			row, err := store.GetAssetBySlug(ctx, tc.slug)
			if err != nil {
				t.Fatalf("GetAssetBySlug(%s): %v", tc.slug, err)
			}
			checkPrice(t, "detail "+tc.name, row.PriceUSD, tc.wantLatest)
		}
	})

	t.Run("PriceHistory24h", func(t *testing.T) {
		for _, tc := range cases {
			pts, err := store.GetAssetPriceHistory24h(ctx, tc.assetID)
			if err != nil {
				t.Fatalf("GetAssetPriceHistory24h(%s): %v", tc.assetID, err)
			}
			checkSeries(t, tc.name, pts, tc.wantLatest, tc.wantEarly)
		}
	})

	t.Run("PriceHistory7d", func(t *testing.T) {
		for _, tc := range cases {
			pts, err := store.GetAssetPriceHistory7d(ctx, tc.assetID)
			if err != nil {
				t.Fatalf("GetAssetPriceHistory7d(%s): %v", tc.assetID, err)
			}
			checkSeries(t, tc.name, pts, tc.wantLatest, tc.wantEarly)
		}
	})

	t.Run("PriceHistory24hBatch", func(t *testing.T) {
		got, err := store.GetAssetsPriceHistory24hBatch(ctx, []string{zaudID, zuscID, zsacID})
		if err != nil {
			t.Fatalf("GetAssetsPriceHistory24hBatch: %v", err)
		}
		for _, tc := range cases {
			checkSeries(t, tc.name, got[tc.assetID], tc.wantLatest, tc.wantEarly)
		}
	})

	t.Run("PriceHistory7dBatch", func(t *testing.T) {
		got, err := store.GetAssetsPriceHistory7dBatch(ctx, []string{zaudID, zuscID, zsacID})
		if err != nil {
			t.Fatalf("GetAssetsPriceHistory7dBatch: %v", err)
		}
		for _, tc := range cases {
			checkSeries(t, tc.name, got[tc.assetID], tc.wantLatest, tc.wantEarly)
		}
	})
}
