// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// Finding M2 — the ~10 serve paths that read a RAW quote/base ratio but were
// NOT applying aggregate.AdjustPrice before serving. flaggedAsset is declared
// decimals()=9 throughout (matching the existing guard tests), so the
// correction factor is K = 10^(9−7) = 100 against a 7dp quote leg. Each test
// asserts (a) the flagged pair now serves the K-scaled value, and (b) an
// unflagged (7dp/7dp) pair is byte-identical — the AdjustPrice no-op contract.
//
// Non-vacuity was demonstrated by neutralizing the fix (making
// normalizeRawRatioString / normalizeTradeRowPrices return their input, and
// marketCapPoints divide by 10^7): every assertion below flips RED. See
// M2_ANALYSIS.md.

var errM2PriceAtMiss = errors.New("m2 test: no bucket")

// m2PriceAtStub implements v1.PriceAtReader keyed on "<base>/<quote>". When
// `historical` is set it returns `current` for a near-now ts and `historical`
// for an older ts, so /v1/price/changes horizons see a non-trivial delta.
type m2PriceAtStub struct {
	byPair     map[string]string
	current    string
	historical string
	histPair   string
	bucketAt   time.Time
}

func (s m2PriceAtStub) PriceAt(_ context.Context, pair canonical.Pair, ts time.Time, _ time.Duration) (string, time.Time, int, error) {
	key := pair.Base.String() + "/" + pair.Quote.String()
	if s.histPair != "" && key == s.histPair {
		if time.Since(ts) < 30*time.Minute {
			return s.current, s.bucketAt, 60, nil
		}
		return s.historical, ts, 60, nil
	}
	if v, ok := s.byPair[key]; ok {
		return v, s.bucketAt, 60, nil
	}
	return "", time.Time{}, 0, errM2PriceAtMiss
}

// ─── /v1/price/at ─────────────────────────────────────────────────────────

func TestPriceAt_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	srv := v1.New(v1.Options{
		PriceAt: m2PriceAtStub{
			byPair:   map[string]string{flaggedAsset + "/fiat:USD": "41.32"},
			bucketAt: ts.Add(-time.Minute),
		},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/price/at?asset="+flaggedAsset+"&quote=fiat:USD&ts="+ts.Format(time.RFC3339))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	// RAW 41.32 × K(=100) = 4132; pre-fix this served the raw "41.32".
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("/v1/price/at not normalized (want 4132.0000000000): %s", body)
	}
}

func TestPriceAt_NonstandardDecimals_7dpByteIdentical(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9) // flagged asset NOT in this pair
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	srv := v1.New(v1.Options{
		PriceAt: m2PriceAtStub{
			byPair:   map[string]string{"native/fiat:USD": "0.1242"},
			bucketAt: ts.Add(-time.Minute),
		},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/price/at?asset=native&quote=fiat:USD&ts="+ts.Format(time.RFC3339))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.1242"`) {
		t.Errorf("7dp /v1/price/at must be byte-identical: %s", body)
	}
}

// ─── /v1/price/changes ─────────────────────────────────────────────────────

func TestPriceChanges_NonstandardDecimals_NormalizesAbsolutesNotPct(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	srv := v1.New(v1.Options{
		PriceAt: m2PriceAtStub{
			histPair:   flaggedAsset + "/fiat:USD",
			current:    "41.32", // now
			historical: "40.00", // every horizon
			bucketAt:   time.Now().UTC().Add(-time.Minute),
		},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/price/changes?asset="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	// Absolute prices scaled by K=100 …
	if !strings.Contains(body, `"current_price":"4132.0000000000"`) {
		t.Errorf("current_price not normalized (want 4132.0000000000): %s", body)
	}
	if !strings.Contains(body, `"reference_price":"4000.0000000000"`) {
		t.Errorf("reference_price not normalized (want 4000.0000000000): %s", body)
	}
	// … but change_pct is scale-invariant: (41.32−40)/40 = +3.30%, IDENTICAL
	// to the raw computation. This is the double-application-free invariant.
	if !strings.Contains(body, `"change_pct":"+3.30"`) {
		t.Errorf("change_pct must be scale-invariant (+3.30): %s", body)
	}
}

// ─── /v1/price/tip (closed-bucket fallback branch) ─────────────────────────

func TestPriceTip_NonstandardDecimals_FallbackNormalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	key := flaggedAsset + "/fiat:USD"
	srv := v1.New(v1.Options{
		// Empty history → tipWindowVWAP finds no trades → falls through to the
		// readPriceWithAliases(s.prices) branch (price_tip.go:168), the M2 gap.
		History: &stubHistoryReader{},
		Prices: &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{key: {
				AssetID: flaggedAsset, Quote: "fiat:USD", Price: "41.32",
				PriceType: "last_trade", ObservedAt: time.Unix(1745000000, 0).UTC(),
			}},
			sources: map[string][]string{key: {"aquarius"}},
		},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/price/tip?asset="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("/v1/price/tip fallback not normalized (want 4132.0000000000): %s", body)
	}
}

// ─── /v1/oracle/x_last_price ───────────────────────────────────────────────

func TestOracleXLastPrice_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	key := flaggedAsset + "/fiat:USD"
	srv := v1.New(v1.Options{
		Prices: &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{key: {
				AssetID: flaggedAsset, Quote: "fiat:USD", Price: "41.32",
				PriceType: "vwap", ObservedAt: time.Unix(1745000000, 0).UTC(),
			}},
			sources: map[string][]string{key: {"aquarius"}},
		},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/oracle/x_last_price?base="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("/v1/oracle/x_last_price not normalized (want 4132.0000000000): %s", body)
	}
}

// ─── /v1/observations (+ the identical /stream row builder) ────────────────

// m2Trade builds a trade for the given pair with raw smallest-unit amounts.
func m2Trade(t *testing.T, base, quote, txHash string, baseAmt, quoteAmt *big.Int) canonical.Trade {
	t.Helper()
	b, err := canonical.ParseAsset(base)
	if err != nil {
		t.Fatalf("ParseAsset base: %v", err)
	}
	q, err := canonical.ParseAsset(quote)
	if err != nil {
		t.Fatalf("ParseAsset quote: %v", err)
	}
	pair, err := canonical.NewPair(b, q)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return canonical.Trade{
		Source: "aquarius", Ledger: 1, TxHash: txHash,
		Timestamp:   time.Unix(1_772_000_000, 0).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(baseAmt),
		QuoteAmount: canonical.NewAmount(quoteAmt),
	}
}

func TestObservations_NonstandardDecimals_NormalizesPrice(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	// base 100 tokens @9dp = 100×10^9; quote 250 @7dp = 250×10^7 → raw ratio
	// 0.025, ×K(100) = true price 2.5. (fiat:USD short-circuits observations,
	// so quote is the 7dp classic USDC.)
	tr := m2Trade(t, flaggedAsset, classicUSDC,
		"0000000000000000000000000000000000000000000000000000000000000001",
		big.NewInt(100_000_000_000), big.NewInt(2_500_000_000))
	srv := v1.New(v1.Options{
		History:             &stubHistoryReader{observations: []canonical.Trade{tr}},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/observations?asset="+flaggedAsset+"&quote="+classicUSDC)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	// Pre-fix served the raw ratio "0.0250000000".
	if !strings.Contains(body, `"price":"2.5000000000"`) {
		t.Errorf("/v1/observations price not normalized (want 2.5000000000): %s", body)
	}
	// Raw on-chain amounts stay smallest-unit (the ADR-0018 contract).
	if !strings.Contains(body, `"base_amount":"100000000000"`) {
		t.Errorf("/v1/observations base_amount must stay raw stroops: %s", body)
	}
}

func TestObservations_NonstandardDecimals_7dpByteIdentical(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9) // flagged asset NOT in this pair
	// native/USDC both 7dp: raw ratio 0.01626 passes through unchanged.
	tr := m2Trade(t, "native", classicUSDC,
		"0000000000000000000000000000000000000000000000000000000000000002",
		big.NewInt(1_000_000_000), big.NewInt(16_260_000))
	srv := v1.New(v1.Options{
		History:             &stubHistoryReader{observations: []canonical.Trade{tr}},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/observations?asset=native&quote="+classicUSDC)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.0162600000"`) {
		t.Errorf("7dp /v1/observations must be byte-identical (0.0162600000): %s", body)
	}
}

// ─── stablecoin-proxy fallback tier (via /v1/price) ────────────────────────

func TestPrice_StablecoinProxy_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	usdc, err := canonical.ParseAsset(classicUSDC)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	// Literal flagged/fiat:USD is ABSENT; the proxy walk rewrites to
	// flagged/<USDC-classic>, whose raw VWAP is 41.32. tryStablecoinFiatProxy
	// must scale it by K=100 → 4132 before serving.
	key := flaggedAsset + "/" + usdc.String()
	srv := v1.New(v1.Options{
		Prices: &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{key: {
				AssetID: flaggedAsset, Quote: usdc.String(), Price: "41.32",
				PriceType: "vwap", ObservedAt: time.Unix(1745000000, 0).UTC(),
			}},
			sources: map[string][]string{key: {"aquarius"}},
		},
		USDPeggedClassics:   []canonical.Asset{usdc},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/price?asset="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("stablecoin-proxy tier not normalized (want 4132.0000000000): %s", body)
	}
}

// ─── /v1/assets/{id} F2: price_usd + market_cap_usd + fdv_usd ──────────────

func TestAssetsF2_NonstandardDecimals_NormalizesPriceAndCaps(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	// Supply: 1000 tokens circulating, 2000 max, both @9dp (stroops).
	circ := new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(9), nil))
	maxSupply := new(big.Int).Mul(big.NewInt(2000), new(big.Int).Exp(big.NewInt(10), big.NewInt(9), nil))
	srv := v1.New(v1.Options{
		Prices: &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{flaggedAsset + "/fiat:USD": {
				AssetID: flaggedAsset, Quote: "fiat:USD", Price: "41.32", PriceType: "vwap",
			}},
		},
		Supply:              &stubSupplyLooker{hit: true, snap: supply.Supply{CirculatingSupply: circ, MaxSupply: maxSupply}},
		TokenDecimals:       &decStub{d: 9, found: true}, // detail.Decimals = 9 (supply divisor)
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/assets/"+flaggedAsset)
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	body, _ := readAll(resp)
	// price_usd: raw 41.32 × K(100) = 4132. Pre-fix served 41.32.
	if !strings.Contains(body, `"price_usd":"4132.0000000000"`) {
		t.Errorf("price_usd not normalized (want 4132.0000000000): %s", body)
	}
	// market_cap = 1000 tokens × $4132 = $4,132,000. Pre-fix: 1000 × 41.32 = 41,320.
	if !strings.Contains(body, `"market_cap_usd":"4132000.00"`) {
		t.Errorf("market_cap_usd not normalized (want 4132000.00): %s", body)
	}
	// fdv = 2000 tokens × $4132 = $8,264,000.
	if !strings.Contains(body, `"fdv_usd":"8264000.00"`) {
		t.Errorf("fdv_usd not normalized (want 8264000.00): %s", body)
	}
}

func TestAssetsF2_7dpUnchanged(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9) // flagged asset NOT this pair
	srv := v1.New(v1.Options{
		Prices: &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{"native/fiat:USD": {
				AssetID: "native", Quote: "fiat:USD", Price: "0.07", PriceType: "vwap",
			}},
		},
		Supply:              &stubSupplyLooker{hit: true, snap: xlmSupplySnap()},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/assets/native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	// Identical to TestF2_NativeAssetWithSupplyAndPrice — wiring the cache is a no-op.
	if !strings.Contains(body, `"price_usd":"0.07"`) {
		t.Errorf("7dp price_usd must be unchanged: %s", body)
	}
	if !strings.Contains(body, `"market_cap_usd":"3493000000.00"`) {
		t.Errorf("7dp market_cap_usd must be unchanged: %s", body)
	}
}
