// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func fetchSingleOHLCBar(t *testing.T, trades []canonical.Trade) v1.OHLCBar {
	t.Helper()
	ts := httpTestServer(t, v1.New(v1.Options{History: &stubHistoryReader{trades: trades}}))
	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&outlier_sigma=0")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.OHLCBar `json:"data"`
	}
	mustDecode(t, resp, &env)
	return env.Data
}

func flooredOHLCTrade(base, quote int64, ts time.Time) canonical.Trade {
	tr := mkOHLCTrade(base, quote, ts)
	tr.ClearsNotionalFloor = true
	return tr
}

func TestOHLC_SingleBarAllDustIsBelowFloor(t *testing.T) {
	t0 := time.Unix(1_772_000_000, 0).UTC()
	bar := fetchSingleOHLCBar(t, []canonical.Trade{
		mkOHLCTrade(1, 100, t0),
		mkOHLCTrade(1, 150, t0.Add(time.Second)),
	})
	if !bar.BelowFloor {
		t.Error("below_floor = false, want true: no trade cleared the notional floor")
	}
	if bar.Open != "100.0000000000" || bar.High != "150.0000000000" {
		t.Errorf("o/h = %s/%s, want the dust prices 100/150 (the only ones there are)", bar.Open, bar.High)
	}
}

func TestOHLC_SingleBarMixedPricesFromFlooredTradesOnly(t *testing.T) {
	t0 := time.Unix(1_772_000_000, 0).UTC()
	bar := fetchSingleOHLCBar(t, []canonical.Trade{
		mkOHLCTrade(1, 900, t0),
		flooredOHLCTrade(1, 100, t0.Add(1*time.Second)),
		mkOHLCTrade(1, 5, t0.Add(2*time.Second)),
		flooredOHLCTrade(1, 120, t0.Add(3*time.Second)),
		mkOHLCTrade(1, 700, t0.Add(4*time.Second)),
	})
	if bar.BelowFloor {
		t.Error("below_floor = true, want false: two trades cleared the floor")
	}
	for _, tc := range []struct{ name, got, want string }{
		{"open", bar.Open, "100.0000000000"},
		{"high", bar.High, "120.0000000000"},
		{"low", bar.Low, "100.0000000000"},
		{"close", bar.Close, "120.0000000000"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %s, want %s — a dust trade set a price beside floored ones", tc.name, tc.got, tc.want)
		}
	}
	if bar.TradeCount != 5 || bar.BaseVolume != "5" || bar.QuoteVolume != "1825" {
		t.Errorf("count/v_base/v_quote = %d/%s/%s, want 5/5/1825 (dust still counts)", bar.TradeCount, bar.BaseVolume, bar.QuoteVolume)
	}
}

func TestOHLCSeries_NativeBarServesBelowFloor(t *testing.T) {
	native := mustParseAsset(t, "native")
	usdc := mustParseAsset(t, usdcClassicID)
	pair, _ := canonical.NewPair(native, usdc)
	t0 := fiatParityBucketStart()
	reader := &floorBarsReader{bars: map[string][]v1.OHLCSeriesBar{
		fiatParityPairKey(pair): {
			floorBar(t0, "7.5", "7.5", "7.5", "7.5", "2", "15", 1, true),
			floorBar(t0.Add(time.Hour), "0.18", "0.19", "0.17", "0.18", "10000000000", "1800000000", 40, false),
		},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader}))
	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote="+usdcClassicID+"&interval=1h&from="+
		t0.Format(time.RFC3339)+"&to="+t0.Add(2*time.Hour).Format(time.RFC3339))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Intervals []map[string]json.RawMessage `json:"intervals"`
		} `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data.Intervals) != 2 {
		t.Fatalf("got %d bars, want 2", len(env.Data.Intervals))
	}
	if got := string(env.Data.Intervals[0]["below_floor"]); got != "true" {
		t.Errorf("dust-only bar below_floor = %q, want true", got)
	}
	if got, present := env.Data.Intervals[1]["below_floor"]; present {
		t.Errorf("floored bar carries below_floor = %s, want it omitted", got)
	}
}

func TestFiatSeries_AllDustBucketIsBelowFloor(t *testing.T) {
	usdc := mustParseAsset(t, usdcClassicID)
	native := mustParseAsset(t, "native")
	usd := mustParseAsset(t, "fiat:USD")
	bookPair, _ := canonical.NewPair(native, usdc)
	directPair, _ := canonical.NewPair(native, usd)
	t0 := fiatParityBucketStart()
	reader := &floorBarsReader{bars: map[string][]v1.OHLCSeriesBar{
		fiatParityPairKey(bookPair):   {floorBar(t0, "0.2", "0.2", "0.2", "0.2", "5", "1", 1, true)},
		fiatParityPairKey(directPair): {floorBar(t0, "7.5", "7.5", "7.5", "7.5", "2", "15", 1, true)},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}}))

	if bar := fetchOneFloorBar(t, ts, "native", t0); !bar.BelowFloor {
		t.Error("below_floor = false on a bucket every constituent bar of which is dust, want true")
	}
}

func TestFiatSeries_MixedBucketIsNotBelowFloor(t *testing.T) {
	usdc := mustParseAsset(t, usdcClassicID)
	native := mustParseAsset(t, "native")
	usd := mustParseAsset(t, "fiat:USD")
	bookPair, _ := canonical.NewPair(native, usdc)
	directPair, _ := canonical.NewPair(native, usd)
	t0 := fiatParityBucketStart()
	reader := &floorBarsReader{bars: map[string][]v1.OHLCSeriesBar{
		fiatParityPairKey(bookPair):   {floorBar(t0, "0.18", "0.19", "0.17", "0.18", "10000000000", "1800000000", 40, false)},
		fiatParityPairKey(directPair): {floorBar(t0, "7.5", "7.5", "7.5", "7.5", "2", "15", 1, true)},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}}))

	bar := fetchOneFloorBar(t, ts, "native", t0)
	if bar.BelowFloor {
		t.Error("below_floor = true, want false: one constituent bar cleared the floor")
	}
	if mustRat(t, bar.H).Cmp(mustRat(t, "0.19")) != 0 {
		t.Errorf("h = %s, want 0.19 from the floored bar only", bar.H)
	}
}
