//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAssetBatchReads_AliasCompleteAndTieBroken proves the listing's batch
// readers answer exactly what the single-asset detail readers do, and that
// a same-minute USDC and fiat:USD print resolves to fiat:USD every time on a
// direct arm. XLM's own price is the anchor, which instead weights that
// minute by volume_usd: $50 at 0.5 and $4 at 0.4 -> 26.6/54 = 0.49259.
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
	assertPointAt(t, "24h", batch24[key], t1.Truncate(time.Hour), 0.4926)
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
	assertPointAt(t, "7d", batch7[key], t1.Truncate(24*time.Hour), 0.4926)

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
	// t1 is 2 h old, past the anchor's freshness bound.
	if row.PriceUSD != nil {
		t.Errorf("GetNativeAssetRow.PriceUSD = %s, want unpriced (newest XLM/USD is 2 h old)", *row.PriceUSD)
	}

	// An XLM hop is priced by the anchor alone, so it agrees with the native
	// row: no route, never t1's raw 0.4 fiat:USD row.
	if tps, err := store.TransitiveUSDPriceCandidates(ctx, tkn.String()); err != nil || len(tps) != 0 {
		t.Errorf("TransitiveUSDPriceCandidates(TKN) = %+v, %v; want no route while XLM/USD is stale", tps, err)
	}
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

// TestAssetsListing_SorobanTypeAndQ pins that a Soroban-native
// contract asset has NULL code/issuer_g_strkey/slug (contract assets
// have no SEP-1 code or issuer account — see listAssetsBaseSelect's
// discovered-contract arm), so the q predicate's original
// `COALESCE(ca.slug, ca.code)` was NULL for every such row and
// `type=soroban&q=<anything>` matched zero rows unconditionally,
// regardless of whether the contract id itself matched. The fix folds
// ca.asset_id into that COALESCE, mirroring the base SELECT's own
// "slug" column, so a query on the contract id (or a prefix of it)
// finds the row.
func TestAssetsListing_SorobanTypeAndQ(t *testing.T) {
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

	const sorobanSearchContractID = "CHBRPOIGF3CBFNOBM2O4RAK3VRJNVGFYGWWQC5HYFSXMECOSFOGYR5XK"

	now := time.Now().UTC().Truncate(time.Minute)
	if err := store.RecordDiscovered(ctx, discovery.Hit{
		ContractID:        sorobanSearchContractID,
		Kind:              discovery.KindSEP41,
		EventType:         discovery.EventTransfer,
		Ledger:            50_000_000,
		ObservedAtRFC3339: now.Add(-24 * time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("RecordDiscovered: %v", err)
	}
	// listAssetsBaseSelect's discovered-contract arm requires an
	// asset_volume_24h row (bounded by TRADED volume, not discovery).
	seedRawVolume(t, ctx, store.DB(), sorobanSearchContractID, "1000")

	rows, err := store.ListAssetsExt(ctx, timescale.ListAssetsOptions{
		Type:  "soroban",
		Q:     "CHBRPOIGF3", // contract-id prefix; there is no code/slug/issuer to match on
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListAssetsExt: %v", err)
	}
	if len(rows) != 1 || rows[0].AssetID != sorobanSearchContractID {
		t.Fatalf("ListAssetsExt(type=soroban, q=contract-id-prefix) = %+v, want exactly the seeded contract row", rows)
	}

	// A query that matches nothing must still return the empty page, not
	// error — the predicate should still be selective.
	empty, err := store.ListAssetsExt(ctx, timescale.ListAssetsOptions{
		Type:  "soroban",
		Q:     "NOSUCHCONTRACTPREFIX",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListAssetsExt (no match): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListAssetsExt(type=soroban, q=no-match) = %+v, want empty", empty)
	}
}

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
// A reader keying on base/quote = 'native' only silently omits the
// crypto:XLM (CEX) leg — the served volume undercounts and the trade-count
// is 1 instead of 2. The readers must use base/quote = ANY(alias forms) and
// see both legs.
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
	// (all forms) vs the native-only total a native-only reader would produce.
	total := scanVolSum(t, ctx, store, aliases)
	nativeOnly := scanVolSum(t, ctx, store, []string{"native"})

	// Fixture sanity: the crypto:XLM leg must contribute real volume the
	// native-only key misses — otherwise the test is vacuous.
	if !(nativeOnly < total) {
		t.Fatalf("fixture did not create cross-form volume: nativeOnly=%.4f total=%.4f", nativeOnly, total)
	}

	// The reader under test must equal the alias-complete total.
	// A native-only reader returns nativeOnly (< total) → this assertion fails RED.
	got, _, err := store.Volume24hUSDForAsset(ctx, native.String())
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
	// and the crypto:XLM trade must be counted. A base/quote = 'native'
	// filter counts only the native leg → 1, not 2.
	n, err := store.GetAssetTradeCount24h(ctx, native.String())
	if err != nil {
		t.Fatalf("GetAssetTradeCount24h: %v", err)
	}
	if n != 2 {
		t.Errorf("GetAssetTradeCount24h(native) = %d, want 2 (native + crypto:XLM legs)", n)
	}

	// Distinct-markets count: two pairs touch an XLM form (native/USDC and
	// crypto:XLM/fiat:USD). A base/quote = 'native' filter sees only the
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
	// The crypto:XLM pair would be absent entirely if per_pair filtered on
	// 'native'; if the CASE alone were wrong it would surface crypto:XLM as
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
	// is 0.5; the alias-complete high is therefore 3.0. A base = 'native'-only
	// read would report 0.5 — omitting the CEX high entirely.
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

// TestAssetListing_MarketCapUsesTheScaleThePriceIsOn feeds the rollup
// WRITER's output through the one reader that multiplies it.
//
// asset_price_snapshot stores the true-scale price for a confirmed
// non-7-decimals token. The listing's market cap is that price times a
// smallest-unit supply divided by 10^decimals, so the divisor has to be
// the token's real decimals — against a RAW ratio column, the standard 7 would
// be right only by cancellation. Neither half
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
			t.Errorf("%s: price_usd = %s, want %s", id, derefOr(row.PriceUSD), want.price)
		}
		if row.MarketCapUSD == nil || *row.MarketCapUSD != want.marketCap {
			t.Errorf("%s: market_cap_usd = %s, want %s (1,000 tokens x %s)",
				id, derefOr(row.MarketCapUSD), want.marketCap, want.price)
		}
		if row.Decimals != want.decimals {
			t.Errorf("%s: decimals = %d, want %d", id, row.Decimals, want.decimals)
		}
	}

	// The same loop for the DETAIL page, whose row comes from the
	// per-asset SQL instead: that read stays RAW and rounds to 10 + k
	// places, and the handler corrects it. The 18-decimals token is the
	// one a flat 10-place precision floor withholds — raw 1.4e-10 is 1.4
	// quanta on that scale — so it is the one that proves the handler
	// reads the scale the SQL actually rounded to.
	t.Run("detail page price", func(t *testing.T) {
		var detail struct {
			Data v1.AssetDetail `json:"data"`
		}
		getJSON(t, ts.URL+"/v1/assets/"+decimalsEighteenContract, &detail)
		if got := derefOr(detail.Data.PriceUSD); got != "14.0000000000" {
			t.Errorf("/v1/assets/{18dp} price_usd = %s, want 14.0000000000", got)
		}
	})
}

// Two batches inside the registry dedupe TTL, the second with a lower first
// ledger: first_* must take the overall minimum, last_* the overall maximum.
func TestAssetRegistry_BatchRangeKeepsBothEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	store.ResetAssetRegistryDedupeForTest()

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	mk := func(ledger uint32, tail string) c.Trade {
		return c.Trade{
			Source:      "test-range",
			Ledger:      ledger,
			TxHash:      "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbe" + tail,
			Timestamp:   base.Add(time.Duration(ledger) * time.Second),
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
		}
	}
	if err := store.BatchInsertTrades(ctx, []c.Trade{mk(200, "a1"), mk(300, "a2")}); err != nil {
		t.Fatalf("batch 1: %v", err)
	}
	if err := store.BatchInsertTrades(ctx, []c.Trade{mk(100, "b1"), mk(150, "b2")}); err != nil {
		t.Fatalf("batch 2: %v", err)
	}

	var firstSeen, firstTrade, lastSeen, lastTrade uint32
	err = store.DB().QueryRowContext(ctx, `
		SELECT first_seen_ledger, first_trade_ledger, last_seen_ledger, last_trade_ledger
		  FROM classic_assets WHERE asset_id = $1`, usdc.String()).Scan(&firstSeen, &firstTrade, &lastSeen, &lastTrade)
	if err != nil {
		t.Fatalf("read classic_assets: %v", err)
	}
	if firstSeen != 100 || firstTrade != 100 {
		t.Errorf("first_seen/first_trade ledger = %d/%d, want 100/100", firstSeen, firstTrade)
	}
	if lastSeen != 300 || lastTrade != 300 {
		t.Errorf("last_seen/last_trade ledger = %d/%d, want 300/300", lastSeen, lastTrade)
	}
}

// The genuine Franklin Templeton BENJI issuer, and one of the eighteen
// accounts on pubnet issuing an asset that also calls itself BENJI. They
// are here as literals because the whole point of this file is that the
// registry is keyed on (code, issuer) and NEVER on the code: the two rows
// below share a code and are different assets.
const (
	benjiGenuineIssuer      = "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5"
	benjiImpersonatorIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// TestAssetRegistry_HeldButNeverTradedAssetIsRegistered is the regression
// test for the defect this file exists because of.
//
// THE DEFECT. `classic_assets` had exactly one population path: a trade.
// `registerClassicAssetSeen` is called from InsertTrade and
// BatchInsertTrades and nowhere else, and `issuers` is written only from
// inside it. So an asset that is HELD but never traded on the SDEX had no
// registry row, therefore no issuer row, therefore no SEP-1 attestation
// fetch, therefore no RWA candidacy — invisible at every step of the chain.
//
// Franklin Templeton's BENJI is the case that surfaced it. It has more
// trustlines than all eighteen impersonating BENJIs combined and zero rows
// in both tables, because a money-market fund is bought and held rather
// than day-traded. On the production lake: 512,496
// classic assets have a trustline, 199,793 had a registry row.
//
// HOW THIS FAILS WITHOUT THE FIX. Subtest "held asset with no trade is
// registered" drives the ONLY thing the lake can tell us about such an
// asset — that a trustline for it exists — and requires a row in both
// tables afterwards. With no holdings path (the state of this repo before
// migration 0158), or with [timescale.Store.RegisterClassicAssetsHeld]
// neutered to a no-op, it fails on the first assertion: 0 rows in
// classic_assets for the genuine BENJI, and 0 rows in issuers for its
// issuer, while the impersonator that happened to trade is present.
func TestAssetRegistry_HeldButNeverTradedAssetIsRegistered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	genuine, err := c.NewClassicAsset("BENJI", benjiGenuineIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset(genuine): %v", err)
	}
	impersonator, err := c.NewClassicAsset("BENJI", benjiImpersonatorIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset(impersonator): %v", err)
	}

	// The impersonator trades. A registry fed only by trades learns only
	// the only BENJI the registry could ever learn about is the one with a
	// market, not the one with the holders.
	tradeAt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	const tradeLedger = 52_000_000
	insertBenjiTrade(t, ctx, store, impersonator, tradeLedger, tradeAt)

	// The genuine one is only ever HELD. The ledger bracket is what a
	// trustline scan of the lake reports: the ledgers at which trustline
	// entries for this asset were last modified.
	holdFirstAt := tradeAt.Add(-48 * time.Hour)
	holdLastAt := tradeAt.Add(-1 * time.Hour)
	const holdFirstLedger = 51_000_000
	const holdLastLedger = 51_990_000

	t.Run("held asset with no trade is registered", func(t *testing.T) {
		assets, issuers, err := store.RegisterClassicAssetsHeld(ctx, []timescale.ClassicAssetHolding{{
			Asset:       genuine,
			FirstLedger: holdFirstLedger,
			FirstAt:     holdFirstAt,
			LastLedger:  holdLastLedger,
			LastAt:      holdLastAt,
		}})
		if err != nil {
			t.Fatalf("RegisterClassicAssetsHeld: %v", err)
		}
		if assets != 1 {
			t.Errorf("asset rows affected = %d, want 1", assets)
		}
		if issuers != 1 {
			t.Errorf("issuer rows inserted = %d, want 1", issuers)
		}

		row := readRegistryRow(t, ctx, store, genuine.String())
		if !row.found {
			t.Fatalf("classic_assets has NO row for %s — a held-but-never-traded asset is still invisible", genuine.String())
		}
		// The whole attestation chain hangs off this row existing.
		if got := countIssuerRows(t, ctx, store, benjiGenuineIssuer); got != 1 {
			t.Fatalf("issuers rows for %s = %d, want 1 — without it the SEP-1 fetch queue can never offer this issuer and /v1/rwa/assets can never see it",
				benjiGenuineIssuer, got)
		}
		if row.code != "BENJI" || row.issuer != benjiGenuineIssuer {
			t.Errorf("registry row identity = (%q, %q), want (BENJI, %s)", row.code, row.issuer, benjiGenuineIssuer)
		}
		// Migration 0135: the slug IS the fully-qualified asset_id.
		if row.slug != genuine.String() {
			t.Errorf("slug = %q, want %q", row.slug, genuine.String())
		}
	})

	t.Run("identity is (code, issuer), never the code", func(t *testing.T) {
		if !readRegistryRow(t, ctx, store, impersonator.String()).found {
			t.Fatalf("impersonator BENJI missing from the registry — the trade path regressed")
		}
		if got := countRegistryRowsForCode(t, ctx, store, "BENJI"); got != 2 {
			t.Fatalf("classic_assets rows with code BENJI = %d, want 2 — the two issuers must be two distinct assets", got)
		}
	})

	t.Run("holdings evidence never fabricates trade activity", func(t *testing.T) {
		row := readRegistryRow(t, ctx, store, genuine.String())
		// observation_count is the explorer's "Observations" column, the
		// default listing rank, and the input to the scam-triage sweep. A
		// holdings-derived row must not inflate any of them.
		if row.observationCount != 0 {
			t.Errorf("observation_count = %d, want 0 — no trade has been observed for this asset", row.observationCount)
		}
		if row.lastTradeAt.Valid || row.firstTradeAt.Valid {
			t.Errorf("trade columns are set (first=%v last=%v) on an asset that has never traded", row.firstTradeAt, row.lastTradeAt)
		}
		if !row.lastHoldingAt.Valid || !row.firstHoldingAt.Valid {
			t.Errorf("holding columns are NULL (first=%v last=%v) on an asset registered from a holding", row.firstHoldingAt, row.lastHoldingAt)
		}
		if row.firstHoldingLedger.Int64 != holdFirstLedger || row.lastHoldingLedger.Int64 != holdLastLedger {
			t.Errorf("holding ledger bracket = [%d,%d], want [%d,%d]",
				row.firstHoldingLedger.Int64, row.lastHoldingLedger.Int64, holdFirstLedger, holdLastLedger)
		}
		// first_seen_* / last_seen_* are the union over sources, which for
		// a holdings-only row is the holding bracket.
		if row.firstSeenLedger != holdFirstLedger || row.lastSeenLedger != holdLastLedger {
			t.Errorf("seen ledger bracket = [%d,%d], want [%d,%d]",
				row.firstSeenLedger, row.lastSeenLedger, holdFirstLedger, holdLastLedger)
		}
	})

	t.Run("re-registering the same holding is idempotent and monotone", func(t *testing.T) {
		// A resumed or re-run backfill re-covers ground. It must converge,
		// not accumulate — and a LATER holding observation must not push
		// first_holding_ledger forward.
		if _, _, err := store.RegisterClassicAssetsHeld(ctx, []timescale.ClassicAssetHolding{{
			Asset:       genuine,
			FirstLedger: holdFirstLedger + 500_000,
			FirstAt:     holdFirstAt.Add(24 * time.Hour),
			LastLedger:  holdLastLedger + 1,
			LastAt:      holdLastAt.Add(time.Minute),
		}}); err != nil {
			t.Fatalf("RegisterClassicAssetsHeld (rerun): %v", err)
		}
		row := readRegistryRow(t, ctx, store, genuine.String())
		if row.observationCount != 0 {
			t.Errorf("observation_count = %d after a rerun, want 0", row.observationCount)
		}
		if row.firstHoldingLedger.Int64 != holdFirstLedger {
			t.Errorf("first_holding_ledger = %d after a later observation, want %d (LEAST must hold)",
				row.firstHoldingLedger.Int64, holdFirstLedger)
		}
		if row.lastHoldingLedger.Int64 != holdLastLedger+1 {
			t.Errorf("last_holding_ledger = %d, want %d (GREATEST must advance)",
				row.lastHoldingLedger.Int64, holdLastLedger+1)
		}
	})

	t.Run("a first trade fills the trade columns without erasing the holding", func(t *testing.T) {
		store.ResetAssetRegistryDedupeForTest()
		insertBenjiTrade(t, ctx, store, genuine, tradeLedger, tradeAt)

		row := readRegistryRow(t, ctx, store, genuine.String())
		if row.observationCount != 1 {
			t.Errorf("observation_count = %d after its first trade, want 1", row.observationCount)
		}
		if !row.firstTradeAt.Valid || !row.lastTradeAt.Valid {
			t.Fatalf("trade columns still NULL after a trade (first=%v last=%v) — LEAST/GREATEST must fill a NULL side",
				row.firstTradeAt, row.lastTradeAt)
		}
		if row.lastTradeLedger.Int64 != tradeLedger {
			t.Errorf("last_trade_ledger = %d, want %d", row.lastTradeLedger.Int64, tradeLedger)
		}
		if row.firstHoldingLedger.Int64 != holdFirstLedger {
			t.Errorf("first_holding_ledger = %d after a trade, want %d — the trade path must not touch holding columns",
				row.firstHoldingLedger.Int64, holdFirstLedger)
		}
		// The union widens to cover both sources: the holding is older
		// than the trade, the trade is newer than the last holding.
		if row.firstSeenLedger != holdFirstLedger {
			t.Errorf("first_seen_ledger = %d, want %d (the earlier holding evidence)", row.firstSeenLedger, holdFirstLedger)
		}
		if row.lastSeenLedger != tradeLedger {
			t.Errorf("last_seen_ledger = %d, want %d (the later trade)", row.lastSeenLedger, tradeLedger)
		}
	})

	t.Run("registry stats separate the two populations", func(t *testing.T) {
		// Register one more held-only asset so held_never_traded is not
		// zero after the previous subtest traded the genuine BENJI.
		heldOnly, err := c.NewClassicAsset("HELDONLY", benjiGenuineIssuer)
		if err != nil {
			t.Fatalf("NewClassicAsset(heldOnly): %v", err)
		}
		if _, _, err := store.RegisterClassicAssetsHeld(ctx, []timescale.ClassicAssetHolding{{
			Asset:       heldOnly,
			FirstLedger: holdFirstLedger,
			FirstAt:     holdFirstAt,
			LastLedger:  holdLastLedger,
			LastAt:      holdLastAt,
		}}); err != nil {
			t.Fatalf("RegisterClassicAssetsHeld(heldOnly): %v", err)
		}
		st, err := store.ClassicAssetRegistryStats(ctx)
		if err != nil {
			t.Fatalf("ClassicAssetRegistryStats: %v", err)
		}
		if st.HoldingOnly != 1 {
			t.Errorf("HoldingOnly = %d, want 1 (the held-but-never-traded asset)", st.HoldingOnly)
		}
		if st.TradeOnly != 1 {
			// Only the impersonator: it traded and no holdings scan has
			// seen it. Native XLM never enters this table at all — the
			// registry is classic-only.
			t.Errorf("TradeOnly = %d, want 1 (the impersonator BENJI)", st.TradeOnly)
		}
	})
}

// benjiRegistryRow is one classic_assets row as this test reads it.
type benjiRegistryRow struct {
	found              bool
	code               string
	issuer             string
	slug               string
	observationCount   int64
	firstSeenLedger    int64
	lastSeenLedger     int64
	firstTradeAt       sql.NullTime
	lastTradeAt        sql.NullTime
	lastTradeLedger    sql.NullInt64
	firstHoldingAt     sql.NullTime
	lastHoldingAt      sql.NullTime
	firstHoldingLedger sql.NullInt64
	lastHoldingLedger  sql.NullInt64
}

func readRegistryRow(t *testing.T, ctx context.Context, store *timescale.Store, assetID string) benjiRegistryRow {
	t.Helper()
	var r benjiRegistryRow
	err := store.DB().QueryRowContext(ctx, `
		SELECT code, issuer_g_strkey, COALESCE(slug, ''),
		       observation_count, first_seen_ledger, last_seen_ledger,
		       first_trade_at, last_trade_at, last_trade_ledger,
		       first_holding_at, last_holding_at,
		       first_holding_ledger, last_holding_ledger
		  FROM classic_assets WHERE asset_id = $1`, assetID).Scan(
		&r.code, &r.issuer, &r.slug,
		&r.observationCount, &r.firstSeenLedger, &r.lastSeenLedger,
		&r.firstTradeAt, &r.lastTradeAt, &r.lastTradeLedger,
		&r.firstHoldingAt, &r.lastHoldingAt,
		&r.firstHoldingLedger, &r.lastHoldingLedger,
	)
	if err == sql.ErrNoRows {
		return benjiRegistryRow{}
	}
	if err != nil {
		t.Fatalf("read classic_assets %s: %v", assetID, err)
	}
	r.found = true
	return r
}

func countIssuerRows(t *testing.T, ctx context.Context, store *timescale.Store, gStrkey string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM issuers WHERE g_strkey = $1`, gStrkey).Scan(&n); err != nil {
		t.Fatalf("count issuers %s: %v", gStrkey, err)
	}
	return n
}

func countRegistryRowsForCode(t *testing.T, ctx context.Context, store *timescale.Store, code string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM classic_assets WHERE code = $1`, code).Scan(&n); err != nil {
		t.Fatalf("count classic_assets code=%s: %v", code, err)
	}
	return n
}

// insertBenjiTrade stores one XLM/asset trade, which is the only way the
// trade-fed registry could ever learn an asset exists.
func insertBenjiTrade(t *testing.T, ctx context.Context, store *timescale.Store, asset c.Asset, ledger uint32, ts time.Time) {
	t.Helper()
	pair, err := c.NewPair(c.NativeAsset(), asset)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	// A 64-char hex tx hash unique to the asset, so two trades in this
	// test can never collide on the trades primary key.
	sum := sha256.Sum256([]byte(asset.String()))
	txHash := hex.EncodeToString(sum[:])
	if err := store.InsertTrade(ctx, c.Trade{
		Source:      "test-holdings",
		Ledger:      ledger,
		TxHash:      txHash,
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
		QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
	}); err != nil {
		t.Fatalf("InsertTrade(%s): %v", asset.String(), err)
	}
}

// TestAssetRegistry_DuplicateReplayDoesNotMutateCounters pins the
// end-to-end counter contract:
// replaying a already-stored trade must NOT advance the
// `classic_assets.observation_count` or `last_seen_*` columns,
// even when the in-process dedupe cache is cold (the simulated-
// process-restart shape).
//
// The concern: a backfill operator (or restarted
// indexer) that re-encounters already-stored trades should not
// inflate registry counters by one per replay. The
// `assetRegistryDedupeTTL` fix protects only the same-process
// hot path; the `RowsAffected == 0` guard inside
// [Store.InsertTrade] is what protects post-restart replay. This
// test isolates the post-restart shape by clearing the dedupe
// cache between the original insert and the replay via
// [timescale.Store.ResetAssetRegistryDedupeForTest].
//
// Three subtests pin the contract:
//
//  1. exact replay (same source+ledger+tx_hash+op_index+ts) → no
//     mutation; observation_count stays at 1.
//  2. cosmetic re-key (different ts in the same second)            →
//     a NEW row is inserted, registry advances to 2 (proves the
//     guard is keyed on RowsAffected, not on asset identity).
//  3. forward-progress replay (different ledger, same asset, TTL
//     bypassed) → registry advances correctly, demonstrating the
//     guard does not over-suppress legitimate updates.
//
// Together these three cases prove the registry counter contract
// holds end-to-end across the realistic operator shapes
// (replay-after-restart, distinct-trades-on-same-asset, late
// ledger arrival).
func TestAssetRegistry_DuplicateReplayDoesNotMutateCounters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Use a fixed valid G-strkey for the issuer + a deterministic
	// classic asset; the test is self-contained — no other test
	// touches this asset.
	const issuerG = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", issuerG)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	xlm := c.NativeAsset()
	pair, err := c.NewPair(xlm, usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	// Reset the dedupe cache between scenarios so each subtest
	// starts from the post-restart shape (the risk shape).
	store.ResetAssetRegistryDedupeForTest()

	baseTS := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)

	mkTrade := func(ledger uint32, txTail rune, ts time.Time) c.Trade {
		// 64-char hex tx hash with one mutable trailing char.
		txHash := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbee" + string(txTail)
		return c.Trade{
			Source:      "test-replay",
			Ledger:      ledger,
			TxHash:      txHash,
			OpIndex:     0,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
		}
	}

	// Scenario 1 — exact replay must NOT advance the counter.
	original := mkTrade(52_000_000, 'a', baseTS)
	if err := store.InsertTrade(ctx, original); err != nil {
		t.Fatalf("InsertTrade (original): %v", err)
	}
	gotCount, gotLastLedger := readRegistry(t, store, usdc.String())
	if gotCount != 1 {
		t.Fatalf("after first insert: observation_count = %d, want 1", gotCount)
	}
	if gotLastLedger != 52_000_000 {
		t.Fatalf("after first insert: last_seen_ledger = %d, want 52_000_000", gotLastLedger)
	}

	// Simulate a process restart: cold dedupe cache, same trade
	// hits InsertTrade again. Without the RowsAffected
	// guard the registry hook would fire and observation_count
	// would advance to 2.
	store.ResetAssetRegistryDedupeForTest()
	if err := store.InsertTrade(ctx, original); err != nil {
		t.Fatalf("InsertTrade (replay): %v", err)
	}
	gotCount, gotLastLedger = readRegistry(t, store, usdc.String())
	if gotCount != 1 {
		t.Errorf("after exact replay: observation_count = %d, want 1 (RowsAffected guard regression)", gotCount)
	}
	if gotLastLedger != 52_000_000 {
		t.Errorf("after exact replay: last_seen_ledger = %d, want 52_000_000", gotLastLedger)
	}

	// Scenario 2 — a NEW trade on the same asset DOES advance the
	// counter once the dedupe cache is cleared (proves the guard
	// is keyed on RowsAffected, not on asset identity).
	store.ResetAssetRegistryDedupeForTest()
	newer := mkTrade(52_000_001, 'b', baseTS.Add(time.Second))
	if err := store.InsertTrade(ctx, newer); err != nil {
		t.Fatalf("InsertTrade (newer trade, same asset): %v", err)
	}
	gotCount, gotLastLedger = readRegistry(t, store, usdc.String())
	if gotCount != 2 {
		t.Errorf("after distinct trade: observation_count = %d, want 2", gotCount)
	}
	if gotLastLedger != 52_000_001 {
		t.Errorf("after distinct trade: last_seen_ledger = %d, want 52_000_001", gotLastLedger)
	}

	// Scenario 3 — replaying scenario-2's trade (now with a cold
	// cache) again must NOT advance to 3. This pins the post-
	// restart-stable contract: the registry only ever advances
	// when a new physical trade row is stored.
	store.ResetAssetRegistryDedupeForTest()
	if err := store.InsertTrade(ctx, newer); err != nil {
		t.Fatalf("InsertTrade (replay of newer): %v", err)
	}
	gotCount, gotLastLedger = readRegistry(t, store, usdc.String())
	if gotCount != 2 {
		t.Errorf("after second exact replay: observation_count = %d, want 2 (RowsAffected guard regression)", gotCount)
	}
	if gotLastLedger != 52_000_001 {
		t.Errorf("after second exact replay: last_seen_ledger = %d, want 52_000_001", gotLastLedger)
	}

	// Sanity: trades hypertable should hold exactly 2 distinct rows
	// (the original + the newer; both replays were no-ops via
	// `ON CONFLICT DO NOTHING`). Confirms the test's premise.
	if got := countTrades(t, store, "test-replay"); got != 2 {
		t.Errorf("trades count for source=test-replay = %d, want 2", got)
	}

	_ = strkey.VersionByteAccountID // keep strkey import live if we ever extend
}

// readRegistry returns (observation_count, last_seen_ledger) for
// the supplied asset_id. Fatal on missing row or query error.
func readRegistry(t *testing.T, store *timescale.Store, assetID string) (uint64, uint32) {
	t.Helper()
	const q = `SELECT observation_count, last_seen_ledger FROM classic_assets WHERE asset_id = $1`
	var count uint64
	var lastLedger uint32
	if err := store.DB().QueryRow(q, assetID).Scan(&count, &lastLedger); err != nil {
		t.Fatalf("readRegistry %s: %v", assetID, err)
	}
	return count, lastLedger
}

// countTrades returns the number of trade rows for the given source.
func countTrades(t *testing.T, store *timescale.Store, source string) int {
	t.Helper()
	const q = `SELECT COUNT(*) FROM trades WHERE source = $1`
	var n int
	if err := store.DB().QueryRow(q, source).Scan(&n); err != nil {
		t.Fatalf("countTrades %s: %v", source, err)
	}
	return n
}

// /v1/assets ranking + keyset pagination against a real Timescale.
//
// The defect: the scam gate withheld a directory-flagged issuer's price
// and market cap but the ORDERING never followed, so JFKBANK2 —
// `malicious`/`unsafe`, no price, no market cap, no %-changes — sat at
// #12 on the live /assets page above USDV, MJQ and BRAVO purely on
// $62.32K of 24h volume. These tests pin the two halves of the fix that
// only a real database can prove:
//
//  1. rank_tier really demotes (SQL EXISTS over account_directory.tags,
//     lowercased on both sides), and demotes a flagged asset even when it
//     IS priced and has the highest volume in the set; and
//  2. the keyset cursor still walks the whole set exactly once now that
//     the ORDER BY has a new LEADING key — the failure mode the file's own
//     comment warns about ("the keyset cursor must encode the same value
//     the ORDER BY ranks on, or pagination skips or repeats rows").

// rankAsset is one seeded listing row.
type rankAsset struct {
	code     string
	issuer   string
	volUSD   string // raw asset_volume_24h.vol_usd
	priceUSD string // "" → seed no trade, so price_usd comes back NULL
	obsCount int64
	tags     []string // account_directory tags for the issuer ("" set → no row)
}

// seedRankAsset inserts the classic_assets spine row with an explicit
// observation_count (the seedDirectoryAsset helper hardcodes 10, which
// gives the observation-count order nothing to rank on).
func seedRankAsset(t *testing.T, ctx context.Context, db *sql.DB, assetID, code, issuer string, obsCount int64) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO classic_assets
		 (asset_id, code, issuer_g_strkey, slug, first_seen_at, first_seen_ledger,
		  last_seen_at, last_seen_ledger, observation_count)
		 VALUES ($1, $2, $3, $1, now(), 1, now(), 2, $4)`,
		assetID, code, issuer, obsCount,
	); err != nil {
		t.Fatalf("seed classic_assets %s: %v", assetID, err)
	}
}

func seedDirectoryTags(t *testing.T, ctx context.Context, db *sql.DB, address, name string, tags []string) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO account_directory (address, name, domain, tags, source, synced_at)
		 VALUES ($1, $2, '', $3, 'stellar-expert', now())`,
		address, name, tags,
	); err != nil {
		t.Fatalf("seed account_directory %s: %v", address, err)
	}
}

// seedUSDPrice gives the asset a direct fiat:USD market so the listing's
// price_usd is non-NULL. base_amount 1 / quote_amount P makes the CAGG's
// volume-weighted quote/base ratio exactly P.
func seedUSDPrice(t *testing.T, ctx context.Context, db *sql.DB, nonce int, assetID, price string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades
		    (source, ledger, tx_hash, op_index, ts,
		     base_asset, quote_asset, base_amount, quote_amount, usd_volume)
		VALUES ('sdex', $1, $2, 0, now() - INTERVAL '1 hour',
		        $3, 'fiat:USD', 1::numeric, $4::numeric, 1::numeric)`,
		60_000_000+nonce, fmt.Sprintf("%064x", 900_000+nonce), assetID, price,
	); err != nil {
		t.Fatalf("seed USD trade for %s: %v", assetID, err)
	}
}

// seedRankFixture materialises the shared scenario and returns the
// canonical asset_id per code.
// derefOr renders a *string for an error message. The assertions below
// compare through the pointer and print the value, so a failure names the
// value that was actually wrong rather than a pointer address.
func derefOr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func seedRankFixture(t *testing.T, ctx context.Context, store *timescale.Store, assets []rankAsset) map[string]string {
	db := store.DB()
	t.Helper()
	ids := make(map[string]string, len(assets))
	for i, a := range assets {
		id := mustClassicID(t, a.code, a.issuer)
		ids[a.code] = id
		seedRankAsset(t, ctx, db, id, a.code, a.issuer, a.obsCount)
		// Volume is seeded AFTER the rollup refresh below — see there.
		// Every asset is an honest `market` so the §4-B concentration
		// demote is a no-op here and rank_tier is the only thing moving
		// rows — otherwise a passing test could be crediting the wrong
		// mechanism.
		seedCharacter(t, ctx, db, id, timescale.VolumeCharacterMarket, 0.10)
		if len(a.tags) > 0 {
			seedDirectoryTags(t, ctx, db, a.issuer, a.code+" issuer", a.tags)
		}
		if a.priceUSD != "" {
			seedUSDPrice(t, ctx, db, i, id, a.priceUSD)
		}
	}
	if _, err := db.ExecContext(ctx,
		"CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)"); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	// The listing does not derive price inside the request:
	// price_usd, the three change columns and source_count come from
	// asset_price_snapshot (migration 0154), refreshed alongside
	// asset_volume_24h by the aggregator's 2-minute worker. Refreshing
	// prices_1m alone leaves that rollup EMPTY, which makes every asset
	// unpriced — and an all-unpriced board collapses tier 0 into tier 1,
	// so the unpriced-demotion this file exists to prove silently stops
	// being observable. Same posture as RefreshAssetVolume24h in
	// asset_volume_rollup_test.go: the test has to do the worker's job.
	if err := store.RefreshAssetListingRollups(ctx); err != nil {
		t.Fatalf("RefreshAssetListingRollups: %v", err)
	}
	// Volume goes in LAST, and that ordering is load-bearing. This
	// fixture uses two different kinds of input on purpose: price is
	// REAL (seeded trades, materialised through prices_1m into
	// asset_price_snapshot, so it exercises the production derivation)
	// while 24h volume is SYNTHETIC — a control value chosen to put the
	// flagged asset at the TOP of the raw ordering, which is the only
	// way the demotion is worth proving. RefreshAssetListingRollups
	// refreshes both rollups in one transaction, so running it after
	// seedRawVolume recomputes vol_usd from the trades and discards the
	// control values (FLAGA's 500000 became a derived figure and the
	// ordering stopped testing anything). Seeding volume afterwards
	// keeps the control, and the assertion that the raw chain fact is
	// unaltered stays meaningful.
	for _, a := range assets {
		seedRawVolume(t, ctx, db, ids[a.code], a.volUSD)
	}
	return ids
}

// rankFixture is the scenario both tests share.
//
// Raw 24h volume orders the set FLAGA > NOPRC > GOODA > GOODB > FLAGB >
// GOODC, which is exactly the raw-volume ranking. FLAGA is deliberately
// BOTH flagged and priced and the highest-volume row in the set: if the
// test only used an unpriced flagged asset, the unpriced tier alone would
// carry it and the scam demotion would go unproven.
var rankFixture = []rankAsset{
	{
		code: "FLAGA", issuer: audIssuer, volUSD: "500000", priceUSD: "2.5", obsCount: 6000,
		tags: []string{"malicious", "unsafe"},
	},
	{
		code: "FLAGB", issuer: auddIssuer, volUSD: "9000", obsCount: 5000,
		tags: []string{"UNSAFE"},
	}, // upper-case: the SQL lowercases like the Go predicate
	{
		code: "GOODA", issuer: goodIssuer, volUSD: "100000", priceUSD: "1.0", obsCount: 4000,
		tags: []string{"anchor", "issuer"},
	}, // benign tags must NOT demote
	{code: "GOODB", issuer: realIssuer, volUSD: "50000", priceUSD: "0.5", obsCount: 3000},
	{code: "GOODC", issuer: audrIssuer, volUSD: "1000", priceUSD: "0.25", obsCount: 2000},
	{code: "NOPRC", issuer: washIssuer, volUSD: "200000", obsCount: 1000},
}

// tieFixture adds rows that TIE on both sort keys, which rankFixture
// deliberately does not: every row above has a distinct obsCount and a
// distinct volUSD, so a walk over it never crosses a tie and never
// exercises the keyset predicate's tie-break half. That is why the
// pagination walk passed for months while the observation-count arm
// compared `(observation_count, asset_id) < ($n, $m)` — a same-direction
// row constructor against a mixed-direction ORDER BY, which on a tie
// re-selects rows already served and skips the rest.
//
// Kept SEPARATE from rankFixture rather than appended to it:
// TestAssetsListing_FlaggedAndUnpricedDemotion asserts exact six-element
// orderings against that var and would break for no benefit.
//
// The tie rows are PRICED on purpose. The volume order carries an
// unpriced→tier-1 arm, so unpriced tie rows would be split across rank
// tiers and the volume tie-break would go unexercised.
var tieFixture = append(append([]rankAsset{}, rankFixture...),
	rankAsset{code: "TIEDA", issuer: tieIssuerA, volUSD: "7500", priceUSD: "0.75", obsCount: 2500},
	rankAsset{code: "TIEDB", issuer: tieIssuerB, volUSD: "7500", priceUSD: "0.75", obsCount: 2500},
	rankAsset{code: "TIEDC", issuer: tieIssuerC, volUSD: "7500", priceUSD: "0.75", obsCount: 2500},
)

func listRankOrder(t *testing.T, ctx context.Context, store *timescale.Store, order timescale.AssetsOrder, limit int) []timescale.AssetRow {
	t.Helper()
	rows, err := store.ListAssetsExt(ctx, timescale.ListAssetsOptions{Order: order, Limit: limit})
	if err != nil {
		t.Fatalf("ListAssetsExt: %v", err)
	}
	return rows
}

func codesOf(rows []timescale.AssetRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Code)
	}
	return out
}

// TestAssetsListing_FlaggedAndUnpricedDemotion pins that a
// directory-flagged asset must not outrank ANY unflagged one, and an
// unpriced asset must not outrank a priced one, under the default
// volume-desc listing sort.
func TestAssetsListing_FlaggedAndUnpricedDemotion(t *testing.T) {
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

	seedRankFixture(t, ctx, store, rankFixture)

	rows := listRankOrder(t, ctx, store, timescale.AssetsOrderVolume24hUSDDesc, 100)
	got := codesOf(rows)

	// Tier 0 (unflagged + priced) by adjusted volume desc, then tier 1
	// (unflagged, no price), then tier 2 (flagged) by volume desc.
	want := []string{"GOODA", "GOODB", "GOODC", "NOPRC", "FLAGA", "FLAGB"}
	if len(got) != len(want) {
		t.Fatalf("listing returned %v, want all %d seeded rows %v (annotate + demote never HIDES a row)", got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("listing order = %v, want %v", got, want)
		}
	}

	byCode := map[string]timescale.AssetRow{}
	for _, r := range rows {
		byCode[r.Code] = r
	}
	// The headline: the flagged asset with the HIGHEST volume in
	// the set sits below the lowest-volume unflagged priced asset.
	if pos(got, "FLAGA") < pos(got, "GOODC") {
		t.Errorf("FLAGA ($500k, malicious/unsafe) at %d must rank BELOW GOODC ($1k, unflagged, priced) at %d",
			pos(got, "FLAGA"), pos(got, "GOODC"))
	}
	// ...and it is demoted for being FLAGGED, not for being unpriced:
	// FLAGA carries a price of its own in the row.
	if byCode["FLAGA"].PriceUSD == nil {
		t.Error("FLAGA must be PRICED in the row — otherwise this test only proves the unpriced tier, not the scam demotion")
	}
	if byCode["FLAGA"].RankTier == nil || *byCode["FLAGA"].RankTier != 2 {
		t.Errorf("FLAGA rank_tier = %v, want 2 (directory-flagged)", byCode["FLAGA"].RankTier)
	}
	// Case-insensitive tag match, matching pricingguard.IsDirectoryScamFlagged.
	if byCode["FLAGB"].RankTier == nil || *byCode["FLAGB"].RankTier != 2 {
		t.Errorf("FLAGB rank_tier = %v, want 2 (tag 'UNSAFE' must match case-insensitively)", byCode["FLAGB"].RankTier)
	}
	// A benign directory label (anchor/issuer) must not demote anything.
	if byCode["GOODA"].RankTier == nil || *byCode["GOODA"].RankTier != 0 {
		t.Errorf("GOODA rank_tier = %v, want 0 — benign directory tags (anchor, issuer) must never demote", byCode["GOODA"].RankTier)
	}
	// An unpriced asset does not outrank a priced one, however big its volume.
	if pos(got, "NOPRC") < pos(got, "GOODC") {
		t.Errorf("NOPRC ($200k, no USD price) at %d must rank BELOW GOODC ($1k, priced) at %d",
			pos(got, "NOPRC"), pos(got, "GOODC"))
	}
	// The raw chain fact is untouched — we demote the RANK, never the data.
	if byCode["FLAGA"].Volume24hUSD == nil || *byCode["FLAGA"].Volume24hUSD != "500000" {
		t.Errorf("FLAGA volume_24h_usd = %s, want the unaltered raw 500000", derefOr(byCode["FLAGA"].Volume24hUSD))
	}
	if byCode["FLAGA"].PriceUSD == nil || *byCode["FLAGA"].PriceUSD != "2.5000000000" {
		t.Errorf("FLAGA price_usd = %s, want the unaltered 2.5 (the API layer, not the query, withholds it)", derefOr(byCode["FLAGA"].PriceUSD))
	}

	// The observation-count order demotes flagged rows too ("whatever the
	// active sort key"), while leaving unpriced rows ranked on activity —
	// that order's contract is activity, not market cap.
	obs := codesOf(listRankOrder(t, ctx, store, timescale.AssetsOrderObservationCountDesc, 100))
	wantObs := []string{"GOODA", "GOODB", "GOODC", "NOPRC", "FLAGA", "FLAGB"}
	for i := range wantObs {
		if obs[i] != wantObs[i] {
			t.Fatalf("observation-count order = %v, want %v (flagged last, unpriced still ranked on activity)", obs, wantObs)
		}
	}
}

// TestAssetsListing_KeysetPaginationCoversEveryRow is the risky half:
// the ORDER BY grew a new LEADING key, so the keyset WHERE must compare
// that key first and the cursor must encode it. Walking the listing two
// rows at a time — exactly as the handler does, via the store's own
// EncodeAssetsCursor — must reproduce the single-page order with no
// skipped and no repeated row, for BOTH orders.
func TestAssetsListing_KeysetPaginationCoversEveryRow(t *testing.T) {
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

	// tieFixture, not rankFixture: the walk must cross a TIE in the sort
	// key, which is where a mixed-direction keyset predicate goes wrong
	// and where the long tail actually lives. See tieFixture's comment.
	seedRankFixture(t, ctx, store, tieFixture)

	for _, tc := range []struct {
		name  string
		order timescale.AssetsOrder
	}{
		{"volume_desc", timescale.AssetsOrderVolume24hUSDDesc},
		{"observation_count_desc", timescale.AssetsOrderObservationCountDesc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			whole := codesOf(listRankOrder(t, ctx, store, tc.order, 100))

			const pageSize = 2
			var walked []string
			cursor := ""
			for page := 0; page < 20; page++ {
				rows, err := store.ListAssetsExt(ctx, timescale.ListAssetsOptions{
					Order:  tc.order,
					Cursor: cursor,
					// Overfetch-by-one, exactly like the handler.
					Limit: pageSize + 1,
				})
				if err != nil {
					t.Fatalf("page %d: %v", page, err)
				}
				hasMore := len(rows) > pageSize
				if hasMore {
					rows = rows[:pageSize]
				}
				if len(rows) == 0 {
					break
				}
				// A cursor that does not round-trip through the validator is
				// a 400 at the handler boundary — pin it here too.
				if err := timescale.ValidateAssetsCursor(cursor, tc.order); err != nil {
					t.Fatalf("page %d: emitted cursor %q failed ValidateAssetsCursor: %v", page, cursor, err)
				}
				walked = append(walked, codesOf(rows)...)
				if !hasMore {
					cursor = ""
					break
				}
				cursor = timescale.EncodeAssetsCursor(rows[len(rows)-1], tc.order)
			}
			if cursor != "" {
				t.Fatalf("pagination did not terminate within 20 pages; walked %v", walked)
			}
			if len(walked) != len(whole) {
				t.Fatalf("paginated walk returned %d rows %v, want the %d of the single page %v (skips or dupes)",
					len(walked), walked, len(whole), whole)
			}
			for i := range whole {
				if walked[i] != whole[i] {
					t.Fatalf("paginated walk = %v, want the single-page order %v", walked, whole)
				}
			}
			seen := map[string]int{}
			for _, code := range walked {
				seen[code]++
			}
			for code, n := range seen {
				if n != 1 {
					t.Errorf("%s appeared %d times across pages, want exactly once", code, n)
				}
			}
		})
	}
}

func pos(codes []string, want string) int {
	for i, c := range codes {
		if c == want {
			return i
		}
	}
	return -1
}

// TestAssetsReader exercises DistinctAssets + HasAsset against a
// real Timescale with our migrations applied. Requires the
// `integration` build tag.
func TestAssetsReader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Empty DB → empty list, empty HasAsset.
	got, next, err := store.DistinctAssets(ctx, "", 100)
	if err != nil {
		t.Fatalf("DistinctAssets (empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d assets", len(got))
	}
	if next != "" {
		t.Errorf("next cursor should be empty, got %q", next)
	}
	// NATIVE IS THE ONE EXCEPTION, and it is deliberate. XLM exists on
	// every Stellar network from the genesis ledger, so HasAsset answers
	// it from first principles rather than from trade evidence and is
	// true even here. The assertion this replaces predated that rule.
	//
	// It is not a hollow yes. The asset detail an index serves for native
	// is supply, decimals, markets_count and sep1_status — read from
	// ledger entries, not trades. Futurenet has ONE
	// XLM trade in its whole history and still serves a full native
	// payload, while a trade-windowed existence check would have 404'd
	// the native asset of a network we ask developers to build against.
	if has, _ := store.HasAsset(ctx, c.NativeAsset()); !has {
		t.Error("HasAsset(native) = false; XLM exists on every Stellar network, traded or not")
	}
	// Every other asset must still be absent on an empty index — that is
	// the property the original assertion was protecting, and it stands.
	usdcProbe, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if has, _ := store.HasAsset(ctx, usdcProbe); has {
		t.Error("HasAsset(USDC) = true on an empty db; only native is answered without evidence")
	}

	// Seed 3 assets via trades: XLM, USDC, PHOENIX.
	xlm := c.NativeAsset()
	usdc, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	pho, _ := c.NewSorobanAsset("CBCZGGNOEUZG4CAAE7TGTQQHETZMKUT4OIPFHHPKEUX46U4KXBBZ3GLH")

	for i, pair := range []c.Pair{
		mustPair(xlm, usdc),
		mustPair(pho, usdc),
		mustPair(xlm, pho),
	} {
		tr := c.Trade{
			Source:      "test",
			Ledger:      uint32(52_000_000 + i),
			TxHash:      hexTx(i),
			OpIndex:     0,
			Timestamp:   time.Now().UTC().Truncate(time.Second).Add(time.Duration(i) * time.Second),
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
		}
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}

	// After seeding, the distinct union is {XLM(native), USDC-G..., CBCZ...}.
	got, next, err = store.DistinctAssets(ctx, "", 100)
	if err != nil {
		t.Fatalf("DistinctAssets: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 distinct assets, got %d: %v", len(got), ids(got))
	}
	if next != "" {
		t.Errorf("next cursor should be empty when page not full, got %q", next)
	}

	for _, want := range []c.Asset{xlm, usdc, pho} {
		has, err := store.HasAsset(ctx, want)
		if err != nil {
			t.Fatalf("HasAsset %s: %v", want.String(), err)
		}
		if !has {
			t.Errorf("HasAsset(%s) = false, want true", want.String())
		}
	}
	// And a seeded-but-different asset should NOT be found.
	notInDB, _ := c.NewFiatAsset("EUR")
	if has, _ := store.HasAsset(ctx, notInDB); has {
		t.Error("HasAsset(EUR) should be false")
	}

	// Perf: an unknown classic asset must route through
	// classic_assets PK lookup and return false. The classic_assets
	// table is populated by InsertTrade's registerClassicAssetSeen
	// hook; an asset_id never seen by that hook (e.g. a random
	// 4-char code against a real-but-unrelated G-strkey) must be
	// known-unknown without touching the trades hypertable.
	bogusClassic, err := c.NewClassicAsset(
		"ZZZZ",
		"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
	)
	if err != nil {
		t.Fatalf("NewClassicAsset(ZZZZ-G...): %v", err)
	}
	has, hasErr := store.HasAsset(ctx, bogusClassic)
	if hasErr != nil {
		t.Fatalf("HasAsset(ZZZZ-G...): %v", hasErr)
	}
	if has {
		t.Errorf("HasAsset(%s) = true, want false (bogus classic asset)", bogusClassic.String())
	}

	// The non-classic arm (timescale.Store.hasNonClassicAsset) is a
	// window-bounded, alias-complete probe. Two halves
	// of its contract only a real Timescale can settle, and the
	// package's scripted-driver tests deliberately cannot:
	//
	//   (a) the `= ANY($1)` alias array has to BIND — pgx encodes the
	//       []string as text[] itself, so a shape Postgres rejects
	//       (42883 and friends) shows up only against a live server;
	//   (b) the `ts >= $2` floor has to actually EXCLUDE, which is the
	//       narrowed semantic the fix chose and the thing most likely to
	//       drift back without a test standing on it.

	// (a) native, crypto:XLM and the XLM SAC are one asset under three
	// canonical ids. Only the `native` leg was seeded above; each of the
	// other two spellings must still find it.
	for _, form := range []c.Asset{mustCryptoTest("XLM"), mustSorobanTest(c.XLMSacContractID)} {
		has, err := store.HasAsset(ctx, form)
		if err != nil {
			t.Fatalf("HasAsset(%s): %v", form.String(), err)
		}
		if !has {
			t.Errorf("HasAsset(%s) = false, want true — the seeded trades carry XLM as `native`, "+
				"and all three forms are the same asset (XLM dual-form rule)", form.String())
		}
	}

	// (b) an asset whose only trade predates the recency window reads as
	// absent. This is the deliberate narrowing: HasAsset answers for the
	// population /v1/assets lists (timescale.MarketsRecencyWindow), not
	// for all of history, because "all of history" needed the unbounded
	// hypertable scan that took /v1/assets/native past its 15s budget.
	stale := sorobanFromSeed(t, 9)
	staleTrade := c.Trade{
		Source:      "test",
		Ledger:      51_000_000,
		TxHash:      hexTx(9),
		OpIndex:     0,
		Timestamp:   time.Now().UTC().Add(-timescale.MarketsRecencyWindow - 48*time.Hour),
		Pair:        mustPair(stale, usdc),
		BaseAmount:  c.NewAmount(big.NewInt(1_000)),
		QuoteAmount: c.NewAmount(big.NewInt(12)),
	}
	if err := store.InsertTrade(ctx, staleTrade); err != nil {
		t.Fatalf("InsertTrade (pre-window): %v", err)
	}
	if has, err := store.HasAsset(ctx, stale); err != nil {
		t.Fatalf("HasAsset(%s): %v", stale.String(), err)
	} else if has {
		t.Errorf("HasAsset(%s) = true, want false — its only trade is older than "+
			"MarketsRecencyWindow (%s), which is outside the window this probe answers for",
			stale.String(), timescale.MarketsRecencyWindow)
	}
	// …and the same contract becomes present the moment it trades inside
	// the window, so (b) is a window boundary and not a dead branch.
	freshTrade := staleTrade
	freshTrade.Ledger = 52_900_000
	freshTrade.TxHash = hexTx(10)
	freshTrade.Timestamp = time.Now().UTC().Truncate(time.Second)
	if err := store.InsertTrade(ctx, freshTrade); err != nil {
		t.Fatalf("InsertTrade (in-window): %v", err)
	}
	if has, err := store.HasAsset(ctx, stale); err != nil {
		t.Fatalf("HasAsset(%s) after in-window trade: %v", stale.String(), err)
	} else if !has {
		t.Errorf("HasAsset(%s) = false after an in-window trade, want true", stale.String())
	}
}

func TestAssetsReaderPagination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Seed 5 soroban assets with strkey-valid C-addresses derived
	// from seed bytes. Hand-written literals (e.g. "CA001JYLG…XOWMA") would be
	// 55 chars — one short of the strkey 56-char requirement — and
	// canonical.NewSorobanAsset rejects them.
	// checksum-valid addresses indexed deterministically by seed so
	// pagination ordering stays reproducible.
	assets := []c.Asset{
		sorobanFromSeed(t, 1),
		sorobanFromSeed(t, 2),
		sorobanFromSeed(t, 3),
		sorobanFromSeed(t, 4),
		sorobanFromSeed(t, 5),
	}
	// Seed each as BASE paired with native XLM.
	for i, a := range assets {
		tr := c.Trade{
			Source: "test", Ledger: uint32(52_000_000 + i),
			TxHash: hexTx(i), OpIndex: 0,
			Timestamp:   time.Now().UTC().Truncate(time.Second).Add(time.Duration(i) * time.Second),
			Pair:        mustPair(a, c.NativeAsset()),
			BaseAmount:  c.NewAmount(big.NewInt(1_000)),
			QuoteAmount: c.NewAmount(big.NewInt(12)),
		}
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// Request a page size of 2 — expect 3 pages (2+2+2 where last
	// page includes 1 extra native + 1 overflow = …). Actually
	// with 5 seeded sorobans + 1 native = 6 distinct assets total.
	// Iterate with cursor until next is empty.
	var allSeen []c.Asset
	cursor := ""
	for iter := 0; iter < 10; iter++ {
		page, next, err := store.DistinctAssets(ctx, cursor, 2)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		allSeen = append(allSeen, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(allSeen) != 6 {
		t.Errorf("expected 6 distinct assets across pages, got %d: %v",
			len(allSeen), ids(allSeen))
	}

	// Ordering: ascending by asset string. C... sort after "native".
	for i := 1; i < len(allSeen); i++ {
		if allSeen[i-1].String() >= allSeen[i].String() {
			t.Errorf("not sorted at %d: %q >= %q",
				i, allSeen[i-1].String(), allSeen[i].String())
		}
	}
}

// ─── helpers ──────────────────────────────────────────────────────

func mustPair(base, quote c.Asset) c.Pair {
	p, err := c.NewPair(base, quote)
	if err != nil {
		panic(err)
	}
	return p
}

func mustSorobanTest(id string) c.Asset {
	a, err := c.NewSorobanAsset(id)
	if err != nil {
		panic(err)
	}
	return a
}

func mustCryptoTest(code string) c.Asset {
	a, err := c.NewCryptoAsset(code)
	if err != nil {
		panic(err)
	}
	return a
}

// sorobanFromSeed builds a Soroban asset whose C-strkey encodes a
// 32-byte contract ID whose first byte is `seed`. Produces a valid
// checksum-encoded C-strkey (56 chars) so canonical.NewSorobanAsset
// accepts it. Deterministic: the same seed always yields the same
// address, preserving pagination-order reproducibility.
func sorobanFromSeed(t *testing.T, seed byte) c.Asset {
	t.Helper()
	var raw [32]byte
	raw[0] = seed
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	a, err := c.NewSorobanAsset(s)
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}
	return a
}

func hexTx(i int) string {
	// Deterministic 64-char hex tx-hash for fixture data.
	base := "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	// Swap two chars to differentiate; trades hypertable unique
	// key tolerates duplicates via ON CONFLICT DO NOTHING, but
	// we want distinct rows for counting.
	tail := "0123456789abcdef"[i%16]
	return base[:63] + string(tail)
}

func ids(as []c.Asset) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.String()
	}
	return out
}

// TestEarliestBucket_AgainstTimescale executes the coverage-floor probe
// (timescale.Store.EarliestBucket, the read behind the API's
// `coverage_from` / `outside_coverage` signal) against a real
// TimescaleDB with the migration chain applied.
//
// It exists because the probe fails SILENTLY by design: a read error
// yields "no signal" plus a warning, never a 5xx. The unit tests in
// internal/storage/timescale stop at the guards that run before the
// query, so a column rename, a plan the CAGG's index cannot serve, or a
// bind-shape mismatch would ship the feature inert with every gate
// green. This test is the one place the SQL is run.
//
// Four properties are pinned on the both-orientations read, each the
// thing that would otherwise make the served floor too LATE — the one
// direction of error that turns a quiet window into a false "before
// the history held":
//
//   - alias fold: a market stored under the CEX spelling (crypto:XLM)
//     is found by its native spelling, and vice versa;
//   - direction fold: a market stored only in the reverse orientation
//     is found by the requested one, with the same floor;
//   - lower bound: a `from` above the first bucket returns the next
//     one, so the window really is applied;
//   - closed-bucket guard: a pair whose only daily bucket is still
//     open reports no floor at all.
//
// The stored-orientation read (EarliestBucketAsStored) is then pinned
// to span ONE orientation, and the quote-literal read
// (EarliestBucketLiteralQuote) to span ONE quote spelling — the read
// the fiat-quoted /v1/ohlc series takes, whose combine names each
// constituent's quote in a single form. Both narrowings are executed
// against the migrated schema for the same reason the wide read is: the
// bound arrays differ, and a bind PostgreSQL rejects would be swallowed
// as "no signal".
//
// Finally the reads are exercised through the served surfaces:
// /v1/ohlc measures a fiat-quoted pair over its USD-pegged
// constituents, and /v1/history measures the orientation its page read
// spans — against the real prices_1d, not a double.
func TestEarliestBucket_AgainstTimescale(t *testing.T) {
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
	mustAsset := func(code string) c.Asset {
		t.Helper()
		a, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	usdc := mustAsset("USDC")
	aqua := mustAsset("AQUA")
	fresh := mustAsset("FRSH")
	other := mustAsset("OTHR")
	cryptoXLM, err := c.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	native := c.NativeAsset()

	mustPair := func(base, quote c.Asset) c.Pair {
		t.Helper()
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	const day = 24 * time.Hour
	now := time.Now().UTC()

	// Market A lives ONLY under the CEX spelling (crypto:XLM / USDC), in
	// the requested direction, across two closed days.
	xlmFirst := now.Add(-5 * day)
	xlmSecond := xlmFirst.Add(day)
	// Market B lives ONLY in the reverse orientation (USDC / AQUA), as
	// the SDEX decoder records whichever way the venue quoted it.
	aquaFirst := now.Add(-3 * day)
	// Market C has exactly one trade, inside TODAY's bucket — a bucket
	// that has not closed. Clamped to the bucket's own start so the case
	// is deterministic across a UTC-midnight boundary.
	openTS := now.Add(-time.Second)
	if start := now.Truncate(day); openTS.Before(start) {
		openTS = start
	}

	for i, tr := range []c.Trade{
		mkAPITrade(1, xlmFirst, mustPair(cryptoXLM, usdc), 1_000_000_000, 12_000_000),
		mkAPITrade(2, xlmSecond, mustPair(cryptoXLM, usdc), 1_000_000_000, 12_100_000),
		mkAPITrade(3, aquaFirst, mustPair(usdc, aqua), 10_000_000, 4_000_000_000),
		mkAPITrade(4, openTS, mustPair(fresh, usdc), 1_000_000_000, 5_000_000),
		// Market D is quoted in ONE spelling of XLM (crypto:XLM), so a
		// quote-alias-folded probe finds it from `native` and a
		// quote-literal one does not. XLM's three canonical forms need
		// no registry, which keeps this case free of process-global
		// fixture state.
		mkAPITrade(5, aquaFirst, mustPair(other, cryptoXLM), 10_000_000, 4_000_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade[%d]: %v", i, err)
		}
	}
	// Background workers are off in this harness (see startTimescale),
	// so the daily rung is materialised by hand — the same rung the API
	// probes on, and the one the served floor is a property of.
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1d: %v", err)
	}

	// The API's probe window: the network's first possible bucket up to
	// a Go-side now.
	epoch := time.Date(2015, 9, 30, 0, 0, 0, 0, time.UTC)
	xlmFloor := xlmFirst.Truncate(day)
	aquaFloor := aquaFirst.Truncate(day)

	cases := []struct {
		name      string
		pair      c.Pair
		from      time.Time
		want      time.Time
		wantFound bool
	}{
		// Market A, every spelling and both orientations → one floor.
		{"stored spelling, stored direction", mustPair(cryptoXLM, usdc), epoch, xlmFloor, true},
		{"alias fold: native reads the crypto:XLM market", mustPair(native, usdc), epoch, xlmFloor, true},
		{"alias fold and flipped direction", mustPair(usdc, native), epoch, xlmFloor, true},
		{"flipped direction under the stored spelling", mustPair(usdc, cryptoXLM), epoch, xlmFloor, true},
		// Market B, stored reverse only → found through the flipped arm.
		{"reverse-stored market by the requested direction", mustPair(aqua, usdc), epoch, aquaFloor, true},
		{"reverse-stored market by its stored direction", mustPair(usdc, aqua), epoch, aquaFloor, true},
		// The lower bound is applied: a `from` above the first bucket
		// returns the next one, not the first.
		{"lower bound excludes the first bucket", mustPair(native, usdc), xlmFloor.Add(day), xlmSecond.Truncate(day), true},
		{"lower bound above every bucket", mustPair(native, usdc), now.Add(-day), time.Time{}, false},
		// No rows at all for the pair.
		{"pair absent from the rung", mustPair(other, aqua), epoch, time.Time{}, false},
		// A bucket that has not closed is not a floor.
		{"open bucket only", mustPair(fresh, usdc), epoch, time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := store.EarliestBucket(ctx, tc.pair, timescale.Granularity1d, tc.from, now)
			if err != nil {
				t.Fatalf("EarliestBucket(%s/%s): %v", tc.pair.Base, tc.pair.Quote, err)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v (bucket %v), want %v", found, got, tc.wantFound)
			}
			if !found {
				if !got.IsZero() {
					t.Errorf("bucket = %v on a not-found return, want zero", got)
				}
				return
			}
			if !got.Equal(tc.want) {
				t.Errorf("floor = %v, want %v", got, tc.want)
			}
			if got.Location() != time.UTC {
				t.Errorf("floor location = %v, want UTC", got.Location())
			}
		})
	}

	// The stored-orientation read: market B is found under the
	// orientation it was stored in and under NO other; the alias fold
	// on each leg still applies (market A under its native spelling).
	storedCases := []struct {
		name      string
		pair      c.Pair
		want      time.Time
		wantFound bool
	}{
		{"reverse-stored market by its stored direction", mustPair(usdc, aqua), aquaFloor, true},
		{"reverse-stored market by the requested direction", mustPair(aqua, usdc), time.Time{}, false},
		{"alias fold still applies within the orientation", mustPair(native, usdc), xlmFloor, true},
		{"flipped alias spelling is not found", mustPair(usdc, native), time.Time{}, false},
	}
	for _, tc := range storedCases {
		t.Run("stored orientation/"+tc.name, func(t *testing.T) {
			got, found, err := store.EarliestBucketAsStored(ctx, tc.pair, timescale.Granularity1d, epoch, now)
			if err != nil {
				t.Fatalf("EarliestBucketAsStored(%s/%s): %v", tc.pair.Base, tc.pair.Quote, err)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v (bucket %v), want %v", found, got, tc.wantFound)
			}
			if found && !got.Equal(tc.want) {
				t.Errorf("floor = %v, want %v", got, tc.want)
			}
		})
	}

	// The quote-literal read: the base leg keeps its alias family and
	// both directions are still folded, but the quote leg is the
	// spelling that was asked for and nothing else. Market D is stored
	// under `crypto:XLM` as its quote.
	literalCases := []struct {
		name      string
		pair      c.Pair
		want      time.Time
		wantFound bool
	}{
		{"the stored quote spelling is found", mustPair(other, cryptoXLM), aquaFloor, true},
		{"a sibling quote spelling is not", mustPair(other, native), time.Time{}, false},
		{"the base leg still folds its family", mustPair(cryptoXLM, usdc), xlmFloor, true},
		{"and so does the direction", mustPair(usdc, cryptoXLM), xlmFloor, true},
	}
	for _, tc := range literalCases {
		t.Run("literal quote/"+tc.name, func(t *testing.T) {
			got, found, err := store.EarliestBucketLiteralQuote(ctx, tc.pair, timescale.Granularity1d, epoch, now)
			if err != nil {
				t.Fatalf("EarliestBucketLiteralQuote(%s/%s): %v", tc.pair.Base, tc.pair.Quote, err)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v (bucket %v), want %v", found, got, tc.wantFound)
			}
			if found && !got.Equal(tc.want) {
				t.Errorf("floor = %v, want %v", got, tc.want)
			}
		})
	}
	// The contrast that makes the narrowing observable: the wide read
	// DOES reach market D from the sibling spelling.
	t.Run("literal quote/the wide read still folds the quote family", func(t *testing.T) {
		got, found, err := store.EarliestBucket(ctx, mustPair(other, native), timescale.Granularity1d, epoch, now)
		if err != nil {
			t.Fatalf("EarliestBucket: %v", err)
		}
		if !found || !got.Equal(aquaFloor) {
			t.Errorf("EarliestBucket(OTHR/native) = (%v, %v), want (%v, true) — the fold under test is unobservable", got, found, aquaFloor)
		}
	})

	// The served surfaces, wired the way the binary wires them, against
	// this prices_1d. The request window ends two days before market B's
	// only bucket, so every answer below is empty and the floor is the
	// whole signal.
	srv := v1.New(v1.Options{
		History:           apiHistoryAdapter{s: store},
		CoverageFloor:     apiCoverageFloorAdapter{s: store},
		USDPeggedClassics: []c.Asset{usdc},
	})
	api := httptest.NewServer(srv.Handler())
	t.Cleanup(api.Close)
	window := "&from=" + now.Add(-10*day).Format(time.RFC3339) + "&to=" + now.Add(-5*day).Format(time.RFC3339)

	type served struct {
		CoverageFrom *time.Time `json:"coverage_from"`
		Flags        struct {
			OutsideCoverage bool `json:"outside_coverage"`
		} `json:"flags"`
	}

	// AQUA/fiat:USD holds no bucket under its literal pair; its
	// /v1/ohlc series is combined from the USD-pegged constituents, one
	// of which (AQUA/USDC, stored as USDC/AQUA) does. The floor must be
	// that constituent's, and the window is below it.
	t.Run("served//v1/ohlc fiat quote measures the constituent set", func(t *testing.T) {
		var got served
		getJSON(t, api.URL+"/v1/ohlc?base="+aqua.String()+"&quote=fiat:USD&interval=1d"+window, &got)
		if got.CoverageFrom == nil {
			t.Fatalf("coverage_from absent; want the USDC constituent's %s", aquaFloor.Format(time.RFC3339))
		}
		if !got.CoverageFrom.Equal(aquaFloor) {
			t.Errorf("coverage_from = %s, want %s", got.CoverageFrom.Format(time.RFC3339), aquaFloor.Format(time.RFC3339))
		}
		if !got.Flags.OutsideCoverage {
			t.Errorf("flags.outside_coverage = false for a window ending below the constituent floor")
		}
	})

	// /v1/history reads BOTH stored orientations, so the two spellings of
	// one market answer alike: each carries the same floor and the same
	// flag. This subtest pinned the opposite until the read was folded —
	// the probe spans exactly what the page read reaches, so widening the
	// read widened the probe with it.
	t.Run("served//v1/history measures both orientations alike", func(t *testing.T) {
		var flipped served
		getJSON(t, api.URL+"/v1/history?base="+aqua.String()+"&quote="+usdc.String()+window, &flipped)
		if flipped.CoverageFrom == nil || !flipped.CoverageFrom.Equal(aquaFloor) {
			t.Errorf("AQUA/USDC page coverage_from = %v, want %s — the read folds both orientations, so the flip carries the same floor",
				flipped.CoverageFrom, aquaFloor.Format(time.RFC3339))
		}
		if !flipped.Flags.OutsideCoverage {
			t.Errorf("AQUA/USDC page flags.outside_coverage = false for a window ending below the floor")
		}
		var stored served
		getJSON(t, api.URL+"/v1/history?base="+usdc.String()+"&quote="+aqua.String()+window, &stored)
		if stored.CoverageFrom == nil || !stored.CoverageFrom.Equal(aquaFloor) {
			t.Errorf("USDC/AQUA page coverage_from = %v, want %s", stored.CoverageFrom, aquaFloor.Format(time.RFC3339))
		}
		if !stored.Flags.OutsideCoverage {
			t.Errorf("USDC/AQUA page flags.outside_coverage = false for a window ending below the floor")
		}
	})
}

// apiCoverageFloorAdapter mirrors cmd/stellarindex-api/main.go's
// storeCoverageFloorReader so the served-surface cases above exercise
// the same read path production does.
type apiCoverageFloorAdapter struct{ s *timescale.Store }

func (a apiCoverageFloorAdapter) EarliestBucket(ctx context.Context, pair c.Pair, granularity string, from, to time.Time) (time.Time, bool, error) {
	return a.s.EarliestBucket(ctx, pair, timescale.HistoryGranularity(granularity), from, to)
}

func (a apiCoverageFloorAdapter) EarliestBucketAsStored(ctx context.Context, pair c.Pair, granularity string, from, to time.Time) (time.Time, bool, error) {
	return a.s.EarliestBucketAsStored(ctx, pair, timescale.HistoryGranularity(granularity), from, to)
}

func (a apiCoverageFloorAdapter) EarliestBucketLiteralQuote(ctx context.Context, pair c.Pair, granularity string, from, to time.Time) (time.Time, bool, error) {
	return a.s.EarliestBucketLiteralQuote(ctx, pair, timescale.HistoryGranularity(granularity), from, to)
}

// TestPoolsFilterFoldsOrientationAndAliases executes the /v1/pools
// filters against the real pools_per_source_1h CAGG. SDEX stores XLM/USDC
// and USDC/XLM trades as separate rows, and Soroban DEXes key XLM under
// its SAC, so a filter applied before the orientation fold, or bound as
// one literal spelling, drops real volume from the pair page and the
// asset Liquidity tab while still presenting the figures as totals.
func TestPoolsFilterFoldsOrientationAndAliases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	const (
		usdc   = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		xlmSAC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	)
	now := time.Now().UTC()
	// sdex: 3 trades stored XLM-first ($10 each), 2 stored USDC-first ($20 each).
	for i := range 3 {
		seedGrainTrade(t, ctx, db, 500+i, "sdex", now.Add(-30*time.Minute), "native", usdc, "10", "1", "10")
	}
	for i := range 2 {
		seedGrainTrade(t, ctx, db, 510+i, "sdex", now.Add(-20*time.Minute), usdc, "native", "2", "20", "20")
	}
	// soroswap keys XLM under its SAC.
	seedGrainTrade(t, ctx, db, 520, "soroswap", now.Add(-10*time.Minute), xlmSAC, usdc, "10", "1", "7")
	// A pool touching neither asset, which every filter must exclude.
	seedGrainTrade(t, ctx, db, 530, "sdex", now.Add(-10*time.Minute), "crypto:ETH", "fiat:USD", "1", "20", "20")

	if _, err := db.ExecContext(ctx, `CALL refresh_continuous_aggregate('pools_per_source_1h', NULL, NULL)`); err != nil {
		t.Fatalf("refresh pools_per_source_1h: %v", err)
	}

	type want struct {
		count int64
		vol   float64
	}
	both := map[string]want{"sdex": {5, 70}, "soroswap": {1, 7}}
	sources := []string{"sdex", "soroswap"}
	for name, tc := range map[string]struct {
		filter timescale.PoolsFilter
		want   map[string]want
	}{
		"pair in canonical order":   {timescale.PoolsFilter{Sources: sources, Base: "native", Quote: usdc}, both},
		"pair in reversed order":    {timescale.PoolsFilter{Sources: sources, Base: usdc, Quote: "native"}, both},
		"asset=native":              {timescale.PoolsFilter{Sources: sources, Asset: "native"}, both},
		"asset=SAC":                 {timescale.PoolsFilter{Sources: sources, Asset: xlmSAC}, both},
		"base-only canonical side":  {timescale.PoolsFilter{Sources: sources, Base: "native"}, both},
		"quote-only canonical side": {timescale.PoolsFilter{Sources: sources, Quote: usdc}, both},
		"base-only non-canonical":   {timescale.PoolsFilter{Sources: sources, Base: usdc}, map[string]want{}},
	} {
		for _, order := range []timescale.MarketsOrder{timescale.MarketsOrderVolume24hDesc, timescale.MarketsOrderPair} {
			pools, _, err := store.AllPools(ctx, tc.filter, "", 100, order)
			if err != nil {
				t.Fatalf("%s (order %v): AllPools: %v", name, order, err)
			}
			got := make(map[string]want, len(pools))
			for _, p := range pools {
				if p.Pair.Base.String() == "crypto:ETH" {
					t.Errorf("%s: unrelated ETH/USD pool leaked through the filter", name)
				}
				if _, dup := got[p.Source]; dup {
					t.Errorf("%s: %s returned twice — orientations were not folded", name, p.Source)
				}
				got[p.Source] = want{p.TradeCount24h, numeric(t, p.Volume24hUSD)}
			}
			if len(got) != len(tc.want) {
				t.Errorf("%s (order %v): got venues %v, want %v", name, order, got, tc.want)
			}
			for src, w := range tc.want {
				if got[src] != w {
					t.Errorf("%s (order %v): %s = %+v, want %+v (both orientations, every alias form)",
						name, order, src, got[src], w)
				}
			}
		}
	}
}
