// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
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
	reader := &usdLegReader{usdPriceFor: "crypto:BTC", price: "101100", withheld: true}
	srv := v1.New(v1.Options{Prices: reader})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?base=native&quote=crypto:BTC")
	if status == http.StatusOK {
		t.Fatalf("a withheld BTC leg was published through the cross: %s", body)
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
