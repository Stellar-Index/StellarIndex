// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// derivedAfterProxyFailure has no direct fiat market for its asset, fails every stablecoin-proxy read of it, and holds the two
// through-XLM legs, so the chart answers from the derived series after an observed read failed.
type derivedAfterProxyFailure struct {
	*stubHistoryReader
	asset canonical.Asset
	legs  map[string][]v1.HistoryPoint
}

func (h derivedAfterProxyFailure) HistoryPointsInRange(_ context.Context, p canonical.Pair, _ string, _, _ time.Time, _ int) ([]v1.HistoryPoint, error) {
	isXLM := false
	for _, a := range canonical.AssetAliases(canonical.NativeAsset()) {
		isXLM = isXLM || p.Quote.Equal(a)
	}
	if p.Base.Equal(h.asset) && !isXLM && p.Quote.Type != canonical.AssetFiat {
		return nil, errChartReadBroke
	}
	return h.legs[p.String()], nil
}

// TestChart_DerivedSeriesKeepsObservedReadFailure: a failed observed-market read means a directly observed series may exist,
// so the through-XLM fallback answer must go out stale and no-store, not as a complete cacheable series.
func TestChart_DerivedSeriesKeepsObservedReadFailure(t *testing.T) {
	asset, err := canonical.ParseAsset(clearedAssetID)
	if err != nil {
		t.Fatal(err)
	}
	native := canonical.NativeAsset()
	usd := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}
	day := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	h := derivedAfterProxyFailure{stubHistoryReader: &stubHistoryReader{}, asset: asset, legs: map[string][]v1.HistoryPoint{
		canonical.Pair{Base: asset, Quote: native}.String(): {{Bucket: day, VWAP: "0.02"}},
		canonical.Pair{Base: native, Quote: usd}.String():   {{Bucket: day, VWAP: "0.20"}},
	}}
	ts := startHTTPTest(t, v1.New(v1.Options{History: h, VerifiedCurrencies: newTestCatalogue(t)}).Handler())
	resp := mustGet(t, ts.URL+"/v1/chart?asset="+clearedAssetID+"&quote=fiat:USD&timeframe=1y&granularity=1d")
	var body struct {
		Data  v1.ChartSeries `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data.Points) != 1 || !body.Flags.Triangulated {
		t.Fatalf("points=%+v triangulated=%v, want the one derived bucket", body.Data.Points, body.Flags.Triangulated)
	}
	if !body.Flags.Stale {
		t.Errorf("flags.stale = false, want true after a failed observed read")
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// TestChartFamily_ThinMarketIsNeverServedClean: every /v1/chart price type must treat a sub-floor market the way its
// siblings do. A price series (vwap, twap) is served but flagged thin_market as /v1/vwap, /v1/twap and /v1/ohlc are; a
// market-cap series is withheld, because /v1/assets/{id} never derives a valuation from a thin price. A new price type
// that skips the substance measurement fails here.
func TestChartFamily_ThinMarketIsNeverServedClean(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC) }
	pts := []v1.HistoryPoint{{Bucket: d(1), VWAP: "0.10"}, {Bucket: d(2), VWAP: "0.20"}}
	opts := v1.Options{
		History: &stubHistoryReader{points: pts, twapPoints: pts},
		Supply: &stubSupplyLooker{daily: []timescale.SupplyDayPoint{
			{Bucket: d(1), Circulating: big.NewInt(1_000_0000000)},
		}},
		VerifiedCurrencies: newTestCatalogue(t),
		Substance: pricingguard.NewSubstanceGate(thinStore{thin: map[string]bool{thinAssetID: true}},
			pricingguard.SubstanceGateOptions{}),
	}
	ts := startHTTPTest(t, v1.New(opts).Handler())

	for _, priceType := range []string{"vwap", "twap", "market_cap"} {
		for _, asset := range []string{thinAssetID, clearedAssetID} {
			status, body := fetch(t, ts.URL+"/v1/chart?asset="+asset+"&quote=fiat:USD&price_type="+priceType+
				"&timeframe=1y&granularity=1d")
			if status != http.StatusOK {
				t.Fatalf("%s %s: status %d: %s", priceType, asset, status, body)
			}
			env := decodeMap(t, body)
			flags, _ := env["flags"].(map[string]any)
			data, _ := env["data"].(map[string]any)
			points, _ := data["points"].([]any)
			thin := asset == thinAssetID
			switch {
			case priceType == "market_cap" && thin:
				if len(points) != 0 || data["market_cap_low_liquidity"] != true {
					t.Errorf("market_cap thin: points=%v low_liquidity=%v, want the series withheld", points, data["market_cap_low_liquidity"])
				}
			case thin:
				if len(points) == 0 || flags["thin_market"] != true {
					t.Errorf("%s thin: points=%d flags.thin_market=%v, want a served series flagged thin", priceType, len(points), flags["thin_market"])
				}
			default:
				if len(points) == 0 || flags["thin_market"] != nil || data["market_cap_low_liquidity"] != nil {
					t.Errorf("%s cleared: points=%d flags=%v low_liquidity=%v, want a clean series", priceType, len(points), flags, data["market_cap_low_liquidity"])
				}
			}
		}
	}
}
