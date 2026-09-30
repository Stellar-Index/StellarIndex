//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAssetDetail_AliasCompleteVolumeAndCount proves the W2-tail batch1
// fix: the asset-detail overlay readers must count an asset's volume /
// trade-count across EVERY canonical form of the asset (XLM's native /
// crypto:XLM / SAC split), not the single `native` spelling the caller
// passes after normalizeXLMAliases collapses XLM.
//
// Fixture (the 24h readers see one closed 1-minute bucket ~2h back; the
// ATH additionally reads a prior day of the same two pairs):
//   - native/USDC   (soroswap, USDC recognised as a USD peg) → on-chain
//     SDEX leg, base_asset='native',      usd_volume = 50
//   - crypto:XLM/USD (binance, fiat:USD)                     → CEX leg,
//     base_asset='crypto:XLM',            usd_volume > 0
//
// Pre-fix the readers key on base/quote = 'native' only, so the
// crypto:XLM (CEX) leg is silently omitted — the served volume undercounts
// and the trade-count is 1 instead of 2. Post-fix they use
// base/quote = ANY(alias forms) and see both legs.
func TestAssetDetail_AliasCompleteVolumeAndCount(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Recognise classic USDC as a USD peg so the native/USDC leg lands a
	// non-null usd_volume (mirrors the soroban_volume_test setup).
	spec, err := timescale.NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}, nil)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)

	native := c.NativeAsset()
	cryptoXLM, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}

	nativeUSDC, _ := c.NewPair(native, usdc)  // on-chain SDEX leg
	cexXLMUSD, _ := c.NewPair(cryptoXLM, usd) // CEX (crypto:XLM) leg

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)

	trades := []c.Trade{
		// native/USDC: 100 XLM base, 50 USDC quote → usd_volume = 50.
		mkIntegrationTrade("soroswap", 1, ts, nativeUSDC, 1_000_000_000, 500_000_000),
		// crypto:XLM/fiat:USD (binance CEX): 100 XLM base, 30 USD quote.
		mkIntegrationTrade("binance", 2, ts, cexXLMUSD, 100_000_000, 300_000_000),
	}
	// A prior day on each leg with enough trades and dollar volume to clear
	// the ATH substance floor; outside every 24h reader's window.
	prior := ts.Add(-72 * time.Hour)
	for i := range 3 {
		trades = append(trades,
			// native/USDC: 100 XLM for 50 USDC → day-VWAP 0.5, $150 over 3 trades.
			mkIntegrationTrade("soroswap", 10+i, prior, nativeUSDC, 1_000_000_000, 500_000_000),
			// crypto:XLM/fiat:USD: 20 XLM for 60 USD → day-VWAP 3.0, $180 over 3 trades.
			mkIntegrationTrade("binance", 10+i, prior, cexXLMUSD, 2_000_000_000, 6_000_000_000),
		)
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", tr.Source, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1d: %v", err)
	}

	// The three canonical forms of XLM, in priority order (SAC last).
	aliases := c.AssetAliasStrings(native)
	if len(aliases) != 3 {
		t.Fatalf("expected 3 XLM alias forms, got %d: %v", len(aliases), aliases)
	}

	// Ground truth read directly from prices_1m: the alias-complete total
	// (all forms) vs the native-only total the pre-fix reader produced.
	total := scanVolSum(t, ctx, store, aliases)
	nativeOnly := scanVolSum(t, ctx, store, []string{"native"})

	// Fixture sanity: the crypto:XLM leg must contribute real volume the
	// native-only key misses — otherwise the test is vacuous.
	if !(nativeOnly < total) {
		t.Fatalf("fixture did not create cross-form volume: nativeOnly=%.4f total=%.4f", nativeOnly, total)
	}

	// The reader under test must equal the alias-complete total (post-fix).
	// Pre-fix it returns nativeOnly (< total) → this assertion fails RED.
	got, err := store.Volume24hUSDForAsset(ctx, native.String())
	if err != nil {
		t.Fatalf("Volume24hUSDForAsset: %v", err)
	}
	if v := mustFloat(t, got); v < total-0.001 || v > total+0.001 {
		t.Errorf("Volume24hUSDForAsset(native) = %s (%.4f), want alias-complete total %.4f "+
			"(pre-fix would report native-only %.4f)", got, v, total, nativeOnly)
	}

	// LatestAssetStats mirrors the same alias-complete sum.
	row, err := store.LatestAssetStats(ctx, native.String())
	if err != nil {
		t.Fatalf("LatestAssetStats: %v", err)
	}
	if row.Volume24hUSD == nil {
		t.Fatalf("LatestAssetStats(native).Volume24hUSD = nil, want %.4f", total)
	}
	if v := mustFloat(t, *row.Volume24hUSD); v < total-0.001 || v > total+0.001 {
		t.Errorf("LatestAssetStats(native).Volume24hUSD = %s (%.4f), want %.4f", *row.Volume24hUSD, v, total)
	}

	// Trade-count reads the trades hypertable directly: both the native
	// and the crypto:XLM trade must be counted. Pre-fix (base/quote =
	// 'native') counts only the native leg → 1, not 2.
	n, err := store.GetAssetTradeCount24h(ctx, native.String())
	if err != nil {
		t.Fatalf("GetAssetTradeCount24h: %v", err)
	}
	if n != 2 {
		t.Errorf("GetAssetTradeCount24h(native) = %d, want 2 (native + crypto:XLM legs)", n)
	}

	// Distinct-markets count: two pairs touch an XLM form (native/USDC and
	// crypto:XLM/fiat:USD). Pre-fix (base/quote = 'native') sees only the
	// native/USDC pair → 1.
	mc, err := store.GetAssetMarketsCount(ctx, native.String())
	if err != nil {
		t.Fatalf("GetAssetMarketsCount: %v", err)
	}
	if mc != 2 {
		t.Errorf("GetAssetMarketsCount(native) = %d, want 2", mc)
	}

	// Top markets: both pairs appear and — critically — the crypto:XLM/USD
	// pair is labelled as the asset's OWN market (side 'base', counterparty
	// the USD quote), not mislabelled with crypto:XLM as the counterparty.
	// Pre-fix the crypto:XLM pair is absent entirely (per_pair filters on
	// 'native'); if the CASE alone regressed it would surface crypto:XLM as
	// a counterparty.
	tops, err := store.GetAssetTopMarkets(ctx, native.String(), 5)
	if err != nil {
		t.Fatalf("GetAssetTopMarkets: %v", err)
	}
	if len(tops) != 2 {
		t.Fatalf("GetAssetTopMarkets(native) returned %d markets, want 2: %+v", len(tops), tops)
	}
	for _, m := range tops {
		if m.Side != "base" {
			t.Errorf("market %+v: side = %q, want \"base\" (XLM was the base leg in both pairs)", m, m.Side)
		}
		if m.Counterparty == "crypto:XLM" || m.Counterparty == "native" {
			t.Errorf("market %+v: counterparty is an XLM alias form — the asset's own market was mislabelled", m)
		}
	}

	// Sparkline readers: the rewritten priority-preserving window SQL must
	// execute and, for XLM, produce at least one non-null USD point from
	// the xlm_usd (native/USDC) path.
	assertHasPoint(t, "GetAssetPriceHistory24h", func() ([]timescale.AssetPricePoint, error) {
		return store.GetAssetPriceHistory24h(ctx, native.String())
	})
	assertHasPoint(t, "GetAssetPriceHistory7d", func() ([]timescale.AssetPricePoint, error) {
		return store.GetAssetPriceHistory7d(ctx, native.String())
	})

	// ATH reads prices_1d as MAX(day-VWAP) across ALL forms. On the prior
	// day the crypto:XLM (CEX) leg's day-VWAP is 3.0 and the native/USDC leg
	// is 0.5; the alias-complete high is therefore 3.0. Pre-fix (base =
	// 'native' only) would report 0.5 — omitting the CEX high entirely.
	ath, err := store.GetAssetATH(ctx, native.String())
	if err != nil {
		t.Fatalf("GetAssetATH: %v", err)
	}
	if ath == nil {
		t.Fatalf("GetAssetATH(native) = nil, want a USD-quoted day-high")
	}
	if v := mustFloat(t, ath.USD); v < 2.99 || v > 3.01 {
		t.Errorf("GetAssetATH(native).USD = %s (%.4f), want ~3.0 (crypto:XLM CEX day-VWAP; "+
			"pre-fix native-only would be 0.5)", ath.USD, v)
	}
}

// assertHasPoint runs a sparkline reader and asserts it executes and
// yields at least one non-null price point.
func assertHasPoint(t *testing.T, name string, fn func() ([]timescale.AssetPricePoint, error)) {
	t.Helper()
	pts, err := fn()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	for _, p := range pts {
		if p.P != nil {
			return
		}
	}
	t.Errorf("%s: no non-null price point produced for XLM", name)
}

// scanVolSum returns the trailing-24h SUM(volume_usd) over prices_1m for
// pairs where the asset (in ANY of the given forms) is base or quote.
func scanVolSum(t *testing.T, ctx context.Context, store *timescale.Store, forms []string) float64 {
	t.Helper()
	const q = `
		SELECT COALESCE(SUM(volume_usd), 0)::text
		  FROM prices_1m
		 WHERE (base_asset = ANY($1) OR quote_asset = ANY($1))
		   AND bucket >= now() - INTERVAL '24 hours'
		   AND bucket  < now()`
	var out string
	if err := store.DB().QueryRowContext(ctx, q, forms).Scan(&out); err != nil {
		t.Fatalf("scanVolSum(%v): %v", forms, err)
	}
	return mustFloat(t, out)
}

// TestAssetATH_SubstanceFloor proves a USD day-bucket only sets an ATH when
// its pair cleared the per-day volume and trade-count floor: a lone print at
// an absurd price, or a handful of dust prints, must not become the high.
func TestAssetATH_SubstanceFloor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

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
	liquid, err := c.NewClassicAsset("FOO", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	thin, err := c.NewClassicAsset("BAR", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	liquidUSDC, _ := c.NewPair(liquid, usdc)
	thinUSDC, _ := c.NewPair(thin, usdc)

	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-5 * 24 * time.Hour).Add(time.Hour)
	var trades []c.Trade
	nonce := 0
	add := func(ts time.Time, pair c.Pair, base, quote int64) {
		nonce++
		trades = append(trades, mkIntegrationTrade("soroswap", nonce, ts, pair, base, quote))
	}
	// Normal day: 3 trades of 50 FOO for 50 USDC → VWAP 1.0, $150.
	for range 3 {
		add(day, liquidUSDC, 500_000_000, 500_000_000)
	}
	// One trade at $5,000,000/FOO carrying $5M of volume: clears the volume
	// floor alone, fails the trade-count floor.
	add(day.Add(24*time.Hour), liquidUSDC, 10_000_000, 50_000_000_000_000)
	// Three dust prints at $1,000/FOO worth $0.10 each: fails the volume floor.
	for range 3 {
		add(day.Add(48*time.Hour), liquidUSDC, 1_000, 1_000_000)
	}
	// The thin asset only ever trades dust.
	for range 3 {
		add(day, thinUSDC, 1_000, 1_000_000)
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	for _, v := range []string{"prices_1m", "prices_1d"} {
		if _, err := store.DB().ExecContext(ctx,
			`CALL refresh_continuous_aggregate('`+v+`', NULL, NULL)`); err != nil {
			t.Fatalf("refresh %s: %v", v, err)
		}
	}

	// Fixture sanity: the rejected days exist in prices_1d with a higher VWAP,
	// otherwise the floor assertions below would be vacuous.
	var above int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM prices_1d WHERE base_asset = ANY($1) AND vwap > 1`,
		[]string{liquid.String(), thin.String()}).Scan(&above); err != nil {
		t.Fatalf("fixture sanity: %v", err)
	}
	if above != 3 {
		t.Fatalf("fixture sanity: %d prices_1d rows priced above $1, want 3", above)
	}

	ath, err := store.GetAssetATH(ctx, liquid.String())
	if err != nil {
		t.Fatalf("GetAssetATH(FOO): %v", err)
	}
	if ath == nil {
		t.Fatal("GetAssetATH(FOO) = nil, want the normal day's VWAP")
	}
	if v := mustFloat(t, ath.USD); v < 0.99 || v > 1.01 {
		t.Errorf("GetAssetATH(FOO).USD = %s, want ~1.0 (the only day above the substance floor)", ath.USD)
	}
	if want := day.Truncate(24 * time.Hour).Format("2006-01-02T15:04:05Z"); ath.At != want {
		t.Errorf("GetAssetATH(FOO).At = %s, want %s", ath.At, want)
	}

	athThin, err := store.GetAssetATH(ctx, thin.String())
	if err != nil {
		t.Fatalf("GetAssetATH(BAR): %v", err)
	}
	if athThin != nil {
		t.Errorf("GetAssetATH(BAR) = %+v, want nil (only dust days)", *athThin)
	}

	batch, err := store.GetAssetsATHBatch(ctx, []string{liquid.String(), thin.String()})
	if err != nil {
		t.Fatalf("GetAssetsATHBatch: %v", err)
	}
	if got, ok := batch[liquid.String()]; !ok || mustFloat(t, got.USD) < 0.99 || mustFloat(t, got.USD) > 1.01 {
		t.Errorf("GetAssetsATHBatch[FOO] = %+v (present=%v), want ~1.0", got, ok)
	}
	if got, ok := batch[thin.String()]; ok {
		t.Errorf("GetAssetsATHBatch[BAR] = %+v, want absent (only dust days)", got)
	}
}
