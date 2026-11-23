// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"io"
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

// legHistory answers each source pair's own points.
type legHistory struct {
	*stubHistoryReader
	legs map[string][]v1.HistoryPoint
}

func (h legHistory) HistoryPointsInRange(_ context.Context, p canonical.Pair, _ string, _, _ time.Time, _ int) ([]v1.HistoryPoint, error) {
	return h.legs[p.String()], nil
}

// TestChartMarketCap_MeasuresTheBookUnderTheLatestPrice: the market-cap chart withholds exactly when /v1/assets/{id} would,
// on the book that owns the newest bucket. A dust book that filled one older bucket does not withhold the series; a thin
// book under the latest price does.
func TestChartMarketCap_MeasuresTheBookUnderTheLatestPrice(t *testing.T) {
	asset, err := canonical.ParseAsset(clearedAssetID)
	if err != nil {
		t.Fatal(err)
	}
	d := func(day int) time.Time { return time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC) }
	direct := canonical.Pair{Base: asset, Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}}.String()
	dust := canonical.Pair{Base: asset, Quote: canonical.Asset{Type: canonical.AssetCrypto, Code: "USDT"}}.String()
	for _, tc := range []struct {
		name     string
		legs     map[string][]v1.HistoryPoint
		withheld bool
	}{
		{"dust book filled an older bucket", map[string][]v1.HistoryPoint{
			direct: {{Bucket: d(2), VWAP: "0.20"}},
			dust:   {{Bucket: d(1), VWAP: "0.10"}},
		}, false},
		{"dust book owns the latest bucket", map[string][]v1.HistoryPoint{
			direct: {{Bucket: d(1), VWAP: "0.10"}},
			dust:   {{Bucket: d(2), VWAP: "0.20"}},
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := startHTTPTest(t, v1.New(v1.Options{
				History: legHistory{stubHistoryReader: &stubHistoryReader{}, legs: tc.legs},
				Supply: &stubSupplyLooker{daily: []timescale.SupplyDayPoint{
					{Bucket: d(1), Circulating: big.NewInt(1_000_0000000)},
				}},
				VerifiedCurrencies: newTestCatalogue(t),
				Substance: pricingguard.NewSubstanceGate(thinStore{pairs: map[string]bool{clearedAssetID + "/crypto:USDT": true}},
					pricingguard.SubstanceGateOptions{}),
			}).Handler())
			status, body := fetch(t, ts.URL+"/v1/chart?asset="+clearedAssetID+"&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
			if status != http.StatusOK {
				t.Fatalf("status %d: %s", status, body)
			}
			data, _ := decodeMap(t, body)["data"].(map[string]any)
			points, _ := data["points"].([]any)
			if withheld := len(points) == 0 && data["market_cap_low_liquidity"] == true; withheld != tc.withheld {
				t.Errorf("points=%v low_liquidity=%v, want withheld=%v", points, data["market_cap_low_liquidity"], tc.withheld)
			}
			if !tc.withheld && len(points) != 2 {
				t.Errorf("served %d points, want both buckets", len(points))
			}
		})
	}
}

// TestChartMarketCap_DerivedRouteMeasuresBothLegs: a price derived through XLM stands on two books, the asset/XLM book and
// the XLM/fiat book under the newest crossed bucket. Either one thin withholds the market cap, as /v1/assets/{id} would; a
// thin book that only filled a bucket the cross never emitted does not.
func TestChartMarketCap_DerivedRouteMeasuresBothLegs(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC) }
	assetLeg := clearedAssetID + "/native"
	pivotLeg := "native/fiat:USD"
	legs := map[string][]v1.HistoryPoint{
		assetLeg: {{Bucket: d(1), VWAP: "10"}, {Bucket: d(2), VWAP: "15"}},
		pivotLeg: {{Bucket: d(1), VWAP: "0.4"}, {Bucket: d(2), VWAP: "0.4"}},
		// A pivot proxy book owns a newer bucket the asset leg lacks, so the cross never serves it.
		"native/crypto:USDT": {{Bucket: d(3), VWAP: "0.5"}},
	}
	for _, tc := range []struct {
		name     string
		thin     map[string]bool
		withheld bool
	}{
		{"asset leg thin", map[string]bool{assetLeg: true}, true},
		{"pivot leg thin", map[string]bool{"native/fiat:USD": true, "crypto:XLM/fiat:USD": true}, true},
		{"both legs deep", nil, false},
		{"thin book only past the last crossed bucket", map[string]bool{"native/crypto:USDT": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := startHTTPTest(t, v1.New(v1.Options{
				History: legHistory{stubHistoryReader: &stubHistoryReader{}, legs: legs},
				Supply: &stubSupplyLooker{daily: []timescale.SupplyDayPoint{
					{Bucket: d(1), Circulating: big.NewInt(1_000_0000000)},
				}},
				VerifiedCurrencies: newTestCatalogue(t),
				Substance:          pricingguard.NewSubstanceGate(thinStore{pairs: tc.thin}, pricingguard.SubstanceGateOptions{}),
			}).Handler())
			status, body := fetch(t, ts.URL+"/v1/chart?asset="+clearedAssetID+"&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
			if status != http.StatusOK {
				t.Fatalf("status %d: %s", status, body)
			}
			data, _ := decodeMap(t, body)["data"].(map[string]any)
			points, _ := data["points"].([]any)
			if withheld := len(points) == 0 && data["market_cap_low_liquidity"] == true; withheld != tc.withheld {
				t.Errorf("points=%v low_liquidity=%v, want withheld=%v", points, data["market_cap_low_liquidity"], tc.withheld)
			}
			if !tc.withheld && len(points) != 2 {
				t.Errorf("served %d points, want both crossed buckets", len(points))
			}
		})
	}
}

// failingSubstanceStore errors measuring any book in fail and measures every other one deep.
type failingSubstanceStore struct{ fail map[string]bool }

func (s failingSubstanceStore) measure(bases, quotes []canonical.Asset) (timescale.MarketSubstance, error) {
	for _, b := range bases {
		for _, q := range quotes {
			if s.fail[b.String()+"/"+q.String()] || s.fail[q.String()+"/"+b.String()] {
				return timescale.MarketSubstance{}, errors.New("substance read timeout")
			}
		}
	}
	return thickMarket, nil
}

func (s failingSubstanceStore) PairMarketSubstance(_ context.Context, bases, quotes []canonical.Asset, _ time.Duration) (timescale.MarketSubstance, error) {
	return s.measure(bases, quotes)
}

func (s failingSubstanceStore) PairMarketSubstanceAt(_ context.Context, bases, quotes []canonical.Asset, _ time.Time, _ time.Duration, _ timescale.HistoryGranularity) (timescale.MarketSubstance, error) {
	return s.measure(bases, quotes)
}

// TestChartMarketCap_UnmeasuredBookIsNotCleared: a substance read that failed on either book under the derived price has
// not ruled, so the market cap is never a clean cacheable 200 (served Stale, no-store, or withheld).
func TestChartMarketCap_UnmeasuredBookIsNotCleared(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC) }
	assetLeg := clearedAssetID + "/native"
	legs := map[string][]v1.HistoryPoint{
		assetLeg:          {{Bucket: d(1), VWAP: "10"}, {Bucket: d(2), VWAP: "15"}},
		"native/fiat:USD": {{Bucket: d(1), VWAP: "0.4"}, {Bucket: d(2), VWAP: "0.4"}},
	}
	for _, tc := range []struct {
		name string
		fail map[string]bool
	}{
		{"asset/XLM book unread", map[string]bool{assetLeg: true}},
		{"XLM/USD book unread", map[string]bool{"native/fiat:USD": true, "crypto:XLM/fiat:USD": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := startHTTPTest(t, v1.New(v1.Options{
				History: legHistory{stubHistoryReader: &stubHistoryReader{}, legs: legs},
				Supply: &stubSupplyLooker{daily: []timescale.SupplyDayPoint{
					{Bucket: d(1), Circulating: big.NewInt(1_000_0000000)},
				}},
				VerifiedCurrencies: newTestCatalogue(t),
				Substance:          pricingguard.NewSubstanceGate(failingSubstanceStore{fail: tc.fail}, pricingguard.SubstanceGateOptions{}),
			}).Handler())
			resp := mustGet(t, ts.URL+"/v1/chart?asset="+clearedAssetID+"&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d")
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			m := decodeMap(t, body)
			data, _ := m["data"].(map[string]any)
			flags, _ := m["flags"].(map[string]any)
			points, _ := data["points"].([]any)
			if len(points) > 0 && (flags["stale"] != true || resp.Header.Get("Cache-Control") != "no-store") {
				t.Errorf("unmeasured book served clean: points=%v flags=%v Cache-Control=%q", points, flags, resp.Header.Get("Cache-Control"))
			}
		})
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
