//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAssetBatchReads_AliasCompleteAndTieBroken proves the listing's batch
// readers answer exactly what the single-asset detail readers do, and that
// a same-minute USDC and fiat:USD print resolves to fiat:USD every time.
//
// Fixture:
//   - t1 (~2h ago): native/USDC at 0.5 AND native/fiat:USD at 0.4, same minute
//   - t2 (~5h ago): crypto:XLM/fiat:USD at 3.0 only (XLM's CEX alias form)
//   - 3 days back: 3 native/USDC trades at 0.5 ($150) and 3 crypto:XLM/fiat:USD
//     trades at 3.0 ($180), both clearing the ATH substance floor
//   - t1: TKN/native at 2.0 (priced through the XLM hop), HOP/USDC at 1.0 AND
//     HOP/fiat:USD at 2.0, and LEG/HOP at 1.0 (priced through HOP's tied USD pick)
//
// A batch read keyed on the raw id saw neither the t2 point nor the 3.0
// high; a pick without a quote tie-break served 0.4 or 0.5 at t1 by scan order.
func TestAssetBatchReads_AliasCompleteAndTieBroken(t *testing.T) {
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

	native := c.NativeAsset()
	cryptoXLM, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usdc, err := c.NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	nativeUSDC, _ := c.NewPair(native, usdc)
	nativeUSD, _ := c.NewPair(native, usd)
	cexXLMUSD, _ := c.NewPair(cryptoXLM, usd)
	tkn, err := c.NewClassicAsset("TKN", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	hop, err := c.NewClassicAsset("HOP", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	leg, err := c.NewClassicAsset("LEG", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	tknXLM, _ := c.NewPair(tkn, native)
	hopUSDC, _ := c.NewPair(hop, usdc)
	hopUSD, _ := c.NewPair(hop, usd)
	legHop, _ := c.NewPair(leg, hop)

	t1 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	t2 := t1.Add(-3 * time.Hour)
	prior := t1.Add(-72 * time.Hour)
	trades := []c.Trade{
		mkIntegrationTrade("soroswap", 1, t1, nativeUSDC, 1_000_000_000, 500_000_000), // 0.5
		mkIntegrationTrade("binance", 2, t1, nativeUSD, 1_000_000_000, 400_000_000),   // 0.4
		mkIntegrationTrade("binance", 3, t2, cexXLMUSD, 100_000_000, 300_000_000),     // 3.0
		mkIntegrationTrade("soroswap", 4, t1, tknXLM, 1_000_000_000, 2_000_000_000),   // 2.0
		mkIntegrationTrade("soroswap", 5, t1, hopUSDC, 1_000_000_000, 1_000_000_000),  // 1.0
		mkIntegrationTrade("binance", 6, t1, hopUSD, 1_000_000_000, 2_000_000_000),    // 2.0
		mkIntegrationTrade("soroswap", 7, t1, legHop, 1_000_000_000, 1_000_000_000),   // 1.0
	}
	for i := range 3 {
		trades = append(trades,
			mkIntegrationTrade("soroswap", 10+i, prior, nativeUSDC, 1_000_000_000, 500_000_000),
			mkIntegrationTrade("binance", 20+i, prior, cexXLMUSD, 2_000_000_000, 6_000_000_000),
		)
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", tr.Source, err)
		}
	}
	for _, v := range []string{"prices_1m", "prices_1d"} {
		if _, err := store.DB().ExecContext(ctx,
			`CALL refresh_continuous_aggregate('`+v+`', NULL, NULL)`); err != nil {
			t.Fatalf("refresh %s: %v", v, err)
		}
	}

	// Fixture sanity: both USD forms really share t1's minute.
	var tied int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(DISTINCT quote_asset) FROM prices_1m WHERE base_asset = 'native' AND bucket = $1`,
		t1).Scan(&tied); err != nil {
		t.Fatalf("fixture sanity: %v", err)
	}
	if tied != 2 {
		t.Fatalf("fixture sanity: %d USD quote forms in t1's bucket, want 2", tied)
	}

	key := native.String()
	single24, err := store.GetAssetPriceHistory24h(ctx, key)
	if err != nil {
		t.Fatalf("GetAssetPriceHistory24h: %v", err)
	}
	batch24, err := store.GetAssetsPriceHistory24hBatch(ctx, []string{key})
	if err != nil {
		t.Fatalf("GetAssetsPriceHistory24hBatch: %v", err)
	}
	assertSameSeries(t, "24h", single24, batch24[key])
	assertPointAt(t, "24h", batch24[key], t1.Truncate(time.Hour), 0.4)
	assertPointAt(t, "24h", batch24[key], t2.Truncate(time.Hour), 3.0)

	single7, err := store.GetAssetPriceHistory7d(ctx, key)
	if err != nil {
		t.Fatalf("GetAssetPriceHistory7d: %v", err)
	}
	batch7, err := store.GetAssetsPriceHistory7dBatch(ctx, []string{key})
	if err != nil {
		t.Fatalf("GetAssetsPriceHistory7dBatch: %v", err)
	}
	assertSameSeries(t, "7d", single7, batch7[key])
	assertPointAt(t, "7d", batch7[key], t1.Truncate(24*time.Hour), 0.4)

	ath, err := store.GetAssetATH(ctx, key)
	if err != nil || ath == nil {
		t.Fatalf("GetAssetATH = %v, %v; want the 3.0 high", ath, err)
	}
	athBatch, err := store.GetAssetsATHBatch(ctx, []string{key})
	if err != nil {
		t.Fatalf("GetAssetsATHBatch: %v", err)
	}
	if got, ok := athBatch[key]; !ok || got != *ath {
		t.Errorf("GetAssetsATHBatch[native] = %+v (present=%v), want GetAssetATH's %+v", got, ok, *ath)
	}
	if v := mustFloat(t, ath.USD); v < 2.99 || v > 3.01 {
		t.Errorf("GetAssetATH(native).USD = %s, want ~3.0 (the crypto:XLM day)", ath.USD)
	}

	row, err := store.GetNativeAssetRow(ctx)
	if err != nil {
		t.Fatalf("GetNativeAssetRow: %v", err)
	}
	if row.PriceUSD == nil {
		t.Fatal("GetNativeAssetRow.PriceUSD = nil, want t1's fiat:USD print")
	}
	if v := mustFloat(t, *row.PriceUSD); v < 0.399 || v > 0.401 {
		t.Errorf("GetNativeAssetRow.PriceUSD = %s, want 0.4 (fiat:USD wins a same-minute tie)", *row.PriceUSD)
	}

	// The transitive resolver's XLM/USD and hop/USD picks break the same tie
	// the same way, so a hop route agrees with the native row exactly.
	xlmUSD := mustRat(t, *row.PriceUSD)
	assertOneRoute(t, ctx, store, tkn.String(), native.String(), new(big.Rat).Mul(big.NewRat(2, 1), xlmUSD))
	assertOneRoute(t, ctx, store, leg.String(), hop.String(), big.NewRat(2, 1))
}

func assertOneRoute(t *testing.T, ctx context.Context, store *timescale.Store, asset, hop string, want *big.Rat) {
	t.Helper()
	tps, err := store.TransitiveUSDPriceCandidates(ctx, asset)
	if err != nil {
		t.Fatalf("TransitiveUSDPriceCandidates(%s): %v", asset, err)
	}
	if len(tps) != 1 || tps[0].Hop != hop {
		t.Fatalf("TransitiveUSDPriceCandidates(%s) = %+v, want one route via %s", asset, tps, hop)
	}
	if got := mustRat(t, tps[0].PriceUSD); got.Cmp(want) != 0 {
		t.Errorf("TransitiveUSDPriceCandidates(%s) via %s = %s, want %s", asset, hop, tps[0].PriceUSD, want.FloatString(4))
	}
}

func assertSameSeries(t *testing.T, name string, single, batch []timescale.AssetPricePoint) {
	t.Helper()
	if len(single) != len(batch) {
		t.Fatalf("%s: batch has %d points, single-asset %d", name, len(batch), len(single))
	}
	for i := range single {
		s, b := single[i], batch[i]
		if s.T != b.T || (s.P == nil) != (b.P == nil) || (s.P != nil && *s.P != *b.P) {
			t.Errorf("%s point %d: batch %s=%v, single-asset %s=%v", name, i, b.T, deref(b.P), s.T, deref(s.P))
		}
	}
}

func assertPointAt(t *testing.T, name string, pts []timescale.AssetPricePoint, at time.Time, want float64) {
	t.Helper()
	key := at.Format("2006-01-02T15:04:05Z")
	for _, p := range pts {
		if p.T != key {
			continue
		}
		if p.P == nil {
			t.Errorf("%s point %s = null, want %.4f", name, key, want)
			return
		}
		if v := mustFloat(t, *p.P); v < want-0.001 || v > want+0.001 {
			t.Errorf("%s point %s = %s, want %.4f", name, key, *p.P, want)
		}
		return
	}
	t.Errorf("%s: no point at %s", name, key)
}

func deref(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}
