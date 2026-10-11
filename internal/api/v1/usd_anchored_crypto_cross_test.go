// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// No venue we ingest quotes XLM against BTC, but both have USD prices: XLM/BTC is their ratio.

const (
	xlmUSDLeg = "crypto:XLM/fiat:USD"
	btcUSDLeg = "crypto:BTC/fiat:USD"
	xlmBTCURL = "/v1/price?base=crypto:XLM&quote=crypto:BTC"
)

func cryptoCrossReader(xlm, btc string, xlmAt, btcAt time.Time) *stubPriceReader {
	return &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			xlmUSDLeg: {AssetID: "crypto:XLM", Quote: "fiat:USD", Price: xlm, PriceType: "vwap", WindowSeconds: 60, ObservedAt: v1.WireTime(xlmAt)},
			btcUSDLeg: {AssetID: "crypto:BTC", Quote: "fiat:USD", Price: btc, PriceType: "vwap", WindowSeconds: 60, ObservedAt: v1.WireTime(btcAt)},
		},
		sources: map[string][]string{xlmUSDLeg: {"kraken", "sdex"}, btcUSDLeg: {"binance", "coinbase"}},
	}
}

func TestPriceCryptoCrossIsExactRatioOfUSDLegs(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Minute)
	srv := v1.New(v1.Options{Prices: cryptoCrossReader("0.1011", "101100", at, at)})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+xlmBTCURL)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	for _, want := range []string{`"price":"0.000001"`, `"triangulated":true`, `"stale":false`, "binance", "kraken"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

func TestPriceCryptoCrossOfMisalignedLegsIsStale(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Minute)
	srv := v1.New(v1.Options{Prices: cryptoCrossReader("0.1011", "101100", at, at.Add(-time.Minute))})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+xlmBTCURL)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if !strings.Contains(body, `"stale":true`) {
		t.Errorf("legs from different minutes must serve stale: %s", body)
	}
}

func TestPriceCryptoCrossMissingLegIsNotFound(t *testing.T) {
	at := time.Now().UTC()
	reader := cryptoCrossReader("0.1011", "101100", at, at)
	delete(reader.snapshots, btcUSDLeg)
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	if status, body := getBody(t, ts.URL+xlmBTCURL); status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", status, body)
	}
}

func TestPriceCryptoCrossZeroLegIsNotFound(t *testing.T) {
	at := time.Now().UTC()
	srv := v1.New(v1.Options{Prices: cryptoCrossReader("0.1011", "0", at, at)})
	ts := startHTTPTest(t, srv.Handler())

	if status, body := getBody(t, ts.URL+xlmBTCURL); status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", status, body)
	}
}

func TestPriceCryptoCrossWithheldLegWithholds(t *testing.T) {
	reader := &usdLegReader{usdPriceFor: "native", price: xlmUSDPrice, withheld: true}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?base=native&quote=crypto:BTC")
	if status == http.StatusOK || !strings.Contains(body, "errors/price-withheld") {
		t.Fatalf("status = %d: a withheld XLM leg must withhold the cross, not collapse to a miss: %s", status, body)
	}
}

func TestPriceCryptoCrossSingleVenueLegIsSingleSource(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Minute)
	reader := cryptoCrossReader("0.1011", "101100", at, at)
	reader.sources[btcUSDLeg] = []string{"binance"}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+xlmBTCURL)
	if status != http.StatusOK || !strings.Contains(body, `"single_source":true`) {
		t.Fatalf("status = %d: a cross is only as attested as its thinner leg: %s", status, body)
	}
}

func TestPriceCryptoCrossDeclaredPegQuoteIsNotFound(t *testing.T) {
	at := time.Now().UTC()
	reader := cryptoCrossReader("0.1011", "101100", at, at)
	reader.snapshots["crypto:USDT/fiat:USD"] = v1.PriceSnapshot{AssetID: "crypto:USDT", Quote: "fiat:USD", Price: "1", PriceType: "peg"}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	if status, body := getBody(t, ts.URL+"/v1/price?base=crypto:XLM&quote=crypto:USDT"); status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: a declared peg is not an observation to divide by: %s", status, body)
	}
}

func TestPriceCryptoCrossAliasOfItselfIsNotCrossed(t *testing.T) {
	at := time.Now().UTC()
	srv := v1.New(v1.Options{Prices: cryptoCrossReader("0.1011", "101100", at, at)})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?base=native&quote=crypto:XLM")
	if status == http.StatusOK && strings.Contains(body, `"triangulated":true`) {
		t.Fatalf("XLM was crossed against its own alias: %s", body)
	}
}

func TestPriceCryptoCrossFrozenQuoteLegServesHeldValue(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Minute)
	srv := v1.New(v1.Options{
		Prices:       cryptoCrossReader("0.1011", "50000", at, at),
		Freeze:       frozenPairs{btcUSDLeg: true},
		Triangulated: lkgPairs{btcUSDLeg + "/300": "101100"},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+xlmBTCURL)
	if status != http.StatusOK || !strings.Contains(body, `"price":"0.000001"`) || !strings.Contains(body, `"frozen":true`) {
		t.Fatalf("status = %d: the cross must divide by the held BTC value: %s", status, body)
	}
}

// A nonstandard-decimals token's raw bucket is scaled before it is divided, exactly as
// /v1/price?quote=fiat:USD scales it.
func TestPriceCryptoCrossScalesNonstandardDecimalsLeg(t *testing.T) {
	const token = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
	at := time.Now().UTC().Truncate(time.Minute)
	reader := cryptoCrossReader("0.1011", "101100", at, at)
	reader.snapshots[token+"/fiat:USD"] = v1.PriceSnapshot{AssetID: token, Quote: "fiat:USD", Price: "0.1011", PriceType: "vwap", WindowSeconds: 60, ObservedAt: v1.WireTime(at)}
	reader.sources[token+"/fiat:USD"] = []string{"soroswap", "aquarius"}
	srv := v1.New(v1.Options{Prices: reader, NonstandardDecimals: nonstandardDecimalsCacheWith(t, token, 8)})
	ts := startHTTPTest(t, srv.Handler())

	status, usdBody := getBody(t, ts.URL+"/v1/price?base="+token+"&quote=fiat:USD")
	if status != http.StatusOK {
		t.Fatalf("usd status = %d: %s", status, usdBody)
	}
	var usd struct {
		Data struct {
			Price string `json:"price"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(usdBody), &usd); err != nil {
		t.Fatal(err)
	}
	if usd.Data.Price == "0.1011" {
		t.Fatalf("fixture does not exercise scaling: %s", usdBody)
	}
	want, ok := new(big.Rat).SetString(usd.Data.Price)
	if !ok {
		t.Fatalf("usd price %q", usd.Data.Price)
	}
	want.Quo(want, big.NewRat(101100, 1))

	status, body := getBody(t, ts.URL+"/v1/price?base="+token+"&quote=crypto:BTC")
	if status != http.StatusOK {
		t.Fatalf("cross status = %d: %s", status, body)
	}
	var cross struct {
		Data struct {
			Price string `json:"price"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &cross); err != nil {
		t.Fatal(err)
	}
	got, ok := new(big.Rat).SetString(cross.Data.Price)
	if !ok || got.Cmp(want) != 0 {
		t.Fatalf("cross = %s, want %s (scaled USD price / BTC): %s", cross.Data.Price, want.FloatString(20), body)
	}
}

func TestPriceCryptoCrossFrozenLegServesHeldValue(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Minute)
	srv := v1.New(v1.Options{
		Prices:       cryptoCrossReader("0.1377", "101100", at, at),
		Freeze:       frozenPairs{xlmUSDLeg: true},
		Triangulated: lkgPairs{xlmUSDLeg + "/300": "0.1011"},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+xlmBTCURL)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if !strings.Contains(body, `"price":"0.000001"`) {
		t.Errorf("the cross must divide the held XLM value, not the refused bucket: %s", body)
	}
	if !strings.Contains(body, `"frozen":true`) {
		t.Errorf("a cross over a frozen leg must say so: %s", body)
	}
}

func TestPriceCryptoCrossFrozenLegWithNothingHeldRefuses(t *testing.T) {
	at := time.Now().UTC()
	srv := v1.New(v1.Options{
		Prices:       cryptoCrossReader("0.1377", "101100", at, at),
		Freeze:       frozenPairs{xlmUSDLeg: true},
		Triangulated: lkgPairs{},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+xlmBTCURL)
	if status == http.StatusOK || strings.Contains(body, "0.00000136") {
		t.Fatalf("status = %d: the refused XLM bucket reached the cross: %s", status, body)
	}
}
