// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// CS-040 money-path: the fiat-combine point path (Server.fiatCombinedTrades)
// merges every usdPeggedConstituent into ONE slice fed to aggregate.VWAP —
// and those constituents span source scales. On-chain DEX legs
// (native/USDC-classic) stamp amounts at 7 decimals; CEX legs
// (crypto:XLM/crypto:USDT) at 8. A trade's PRICE (quote/base) is
// scale-invariant, but its WEIGHT in Σquote/Σbase is its raw base amount =
// real_volume × 10^scale, so the 8dp CEX leg is over-weighted 10× versus an
// equal-real-volume 7dp on-chain leg. The served VWAP was therefore dragged
// toward the CEX price. Server.fiatCombinedTrades now lifts every trade to the
// common (max) scale before returning, restoring the true
// real-volume-weighted mean.
//
// Fixture — one 1h bucket, two constituents of the native/fiat:USD target,
// equal REAL volume (1000 XLM each), different scales:
//
//	native/<USDC-classic>   source=sdex    (7dp)  10^10 base / 10^9  quote → 0.10
//	crypto:XLM/crypto:USDT  source=binance (8dp)  10^11 base / 1.2·10^10 quote → 0.12
//
// True equal-volume mean = 0.11. Pre-fix VWAP = (10^9 + 1.2·10^10) /
// (10^10 + 10^11) = 13/110 ≈ 0.1181818… — the 10×-over-weighted-CEX value
// (the wire renders it 0.1181818181, ratToDecimal truncating at 10 digits).

const (
	scaleNormWantVWAP   = "0.1100000000" // real-volume-weighted midpoint (fixed)
	scaleNormPreFixVWAP = "0.1181818181" // 10×-over-weighted CEX (un-normalized)
	// Both legs are 1000 XLM; normalized to the common 8dp scale each is
	// 10^11, so the combined base volume is 2·10^11.
	scaleNormWantBaseVolume = "200000000000"
)

func scaleNormBucketStart() time.Time { return time.Date(2026, 5, 6, 7, 0, 0, 0, time.UTC) }

func scaleNormWindow() string {
	t0 := scaleNormBucketStart()
	return "&from=" + t0.Format(time.RFC3339) + "&to=" + t0.Add(time.Hour).Format(time.RFC3339)
}

// scaleNormTrade builds a trade at an explicit source (so its scale is what
// external.Lookup resolves) with big-int amounts.
func scaleNormTrade(source string, pair canonical.Pair, ledger uint32, ts time.Time, base, quote *big.Int) canonical.Trade {
	return canonical.Trade{
		Source:      source,
		Ledger:      ledger,
		TxHash:      "00000000000000000000000000000000000000000000000000000000000000ab",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(base),
		QuoteAmount: canonical.NewAmount(quote),
	}
}

func scaleNormPow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// TestFiatVWAPMixedScaleNormalized is the CS-040 regression. It goes through
// the real /v1/vwap handler over a mixed-scale constituent set and asserts the
// corrected, real-volume-weighted price.
//
// Non-vacuous by construction: reverting the fix (dropping the
// NormalizeAmountScale call in fiatCombinedTrades) makes this serve
// scaleNormPreFixVWAP and the assertion fails; it uses only pre-existing
// symbols, so it compiles against the un-fixed tree.
func TestFiatVWAPMixedScaleNormalized(t *testing.T) {
	usdc, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	xlmNative, _ := canonical.ParseAsset("native")
	xlmTicker, _ := canonical.ParseAsset("crypto:XLM")
	usdt, _ := canonical.ParseAsset("crypto:USDT")

	onchainPair, _ := canonical.NewPair(xlmNative, usdc) // 7dp, source sdex
	cexPair, _ := canonical.NewPair(xlmTicker, usdt)     // 8dp, source binance
	t0 := scaleNormBucketStart()

	reader := &fiatConstituentReader{
		tradesByPair: map[string][]canonical.Trade{
			// 1000 XLM @0.10 on-chain 7dp: 10^10 base, 10^9 quote.
			fiatParityPairKey(onchainPair): {scaleNormTrade("sdex", onchainPair, 1, t0,
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(7)),
				new(big.Int).Mul(big.NewInt(100), scaleNormPow10(7)))},
			// 1000 XLM @0.12 CEX 8dp: 10^11 base, 1.2·10^10 quote.
			fiatParityPairKey(cexPair): {scaleNormTrade("binance", cexPair, 2, t0.Add(5*time.Minute),
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(8)),
				new(big.Int).Mul(big.NewInt(120), scaleNormPow10(8)))},
		},
	}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD"+scaleNormWindow())
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("vwap status = %d, want 200: %s", resp.StatusCode, body)
	}
	var env struct {
		Data v1.VWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.TradeCount != 2 {
		t.Fatalf("trade_count = %d, want 2 (one on-chain, one CEX constituent)", env.Data.TradeCount)
	}
	if env.Data.Price != scaleNormWantVWAP {
		t.Errorf("vwap price = %q, want %q (equal-real-volume midpoint of 0.10 on-chain "+
			"and 0.12 CEX); %q is the pre-fix value with the 8dp CEX leg over-weighted 10×",
			env.Data.Price, scaleNormWantVWAP, scaleNormPreFixVWAP)
	}
	// Volume is now reported at the common 8dp scale (both legs 1000 XLM →
	// 2000 XLM total). Pre-fix this summed cross-scale integers
	// (10^10 + 10^11 = 1.1·10^11), a physically meaningless quantity.
	if env.Data.BaseVolume != scaleNormWantBaseVolume {
		t.Errorf("vwap base_volume = %q, want %q (2000 XLM at the common 8dp scale)",
			env.Data.BaseVolume, scaleNormWantBaseVolume)
	}
}

// TestFiatVWAPUniformOnChainUnchanged is the byte-identical guard at the
// handler layer: an all-on-chain (uniform 7dp) constituent set must serve the
// exact same VWAP with the normalization in place as without it. Two 7dp
// constituents, 0.10 and 0.20 on equal 1000-XLM base → 0.15 exactly, and the
// raw base volume (2·10^10) is preserved unchanged (no rescale to 8dp).
func TestFiatVWAPUniformOnChainUnchanged(t *testing.T) {
	usdc, _ := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	usdcCrypto, _ := canonical.ParseAsset("crypto:USDC")
	xlmNative, _ := canonical.ParseAsset("native")

	pairA, _ := canonical.NewPair(xlmNative, usdc)       // native/USDC-classic (peg)
	pairB, _ := canonical.NewPair(xlmNative, usdcCrypto) // native/crypto:USDC (backer)
	t0 := scaleNormBucketStart()

	reader := &fiatConstituentReader{
		tradesByPair: map[string][]canonical.Trade{
			// 1000 XLM @0.10, 7dp.
			fiatParityPairKey(pairA): {scaleNormTrade("sdex", pairA, 1, t0,
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(7)),
				new(big.Int).Mul(big.NewInt(100), scaleNormPow10(7)))},
			// 1000 XLM @0.20, 7dp.
			fiatParityPairKey(pairB): {scaleNormTrade("soroswap", pairB, 2, t0.Add(time.Minute),
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(7)),
				new(big.Int).Mul(big.NewInt(200), scaleNormPow10(7)))},
		},
	}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD"+scaleNormWindow())
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("vwap status = %d, want 200: %s", resp.StatusCode, body)
	}
	var env struct {
		Data v1.VWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Price != "0.1500000000" {
		t.Errorf("uniform-7dp vwap price = %q, want 0.1500000000 (0.10 and 0.20 on equal "+
			"volume) — normalization must be a no-op for a single-scale window", env.Data.Price)
	}
	if env.Data.BaseVolume != "20000000000" {
		t.Errorf("uniform-7dp vwap base_volume = %q, want 20000000000 (2·10^10 raw, "+
			"unchanged — a uniform window must not be rescaled)", env.Data.BaseVolume)
	}
}

// TestPriceTipMixedScaleNormalized pins the same fix on the live /v1/price/tip
// surface. tipWindowVWAP MERGES the XLM alias pairs — on-chain native/<q>
// (7dp) with CEX crypto:XLM/<q> (8dp) — into one slice before aggregate.VWAP,
// so an asset cross-listed on both venues had its tip price dragged toward the
// finer-scaled CEX leg. Same fixture as the fiat case: 1000 XLM @0.10 on-chain
// + 1000 XLM @0.12 CEX → true tip 0.11.
//
// Non-vacuous: reverting the NormalizeAmountScale call in tipWindowVWAP serves
// 0.1181818181 and fails this; it uses only pre-existing symbols.
func TestPriceTipMixedScaleNormalized(t *testing.T) {
	xlmNative, _ := canonical.ParseAsset("native")
	xlmTicker, _ := canonical.ParseAsset("crypto:XLM")
	usdt, _ := canonical.ParseAsset("crypto:USDT")

	onchainPair, _ := canonical.NewPair(xlmNative, usdt) // 7dp, source sdex
	cexPair, _ := canonical.NewPair(xlmTicker, usdt)     // 8dp, source binance
	now := time.Now().UTC()

	reader := &fiatConstituentReader{
		tradesByPair: map[string][]canonical.Trade{
			fiatParityPairKey(onchainPair): {scaleNormTrade("sdex", onchainPair, 1, now.Add(-2*time.Second),
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(7)),
				new(big.Int).Mul(big.NewInt(100), scaleNormPow10(7)))},
			fiatParityPairKey(cexPair): {scaleNormTrade("binance", cexPair, 2, now.Add(-1*time.Second),
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(8)),
				new(big.Int).Mul(big.NewInt(120), scaleNormPow10(8)))},
		},
	}
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}, History: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=crypto:XLM&quote=crypto:USDT&window_seconds=5")
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("tip status = %d, want 200: %s", resp.StatusCode, body)
	}
	var env struct {
		Data struct {
			Price     string `json:"price"`
			PriceType string `json:"price_type"`
		} `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.PriceType != "vwap" {
		t.Fatalf("price_type = %q, want vwap (the rolling-window path must be the one exercised)", env.Data.PriceType)
	}
	if env.Data.Price != scaleNormWantVWAP {
		t.Errorf("tip price = %q, want %q (equal-real-volume midpoint); %q is the pre-fix "+
			"value with the 8dp CEX leg over-weighted 10×",
			env.Data.Price, scaleNormWantVWAP, scaleNormPreFixVWAP)
	}
}
