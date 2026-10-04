// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const (
	depegXLMUSDCPair = "native/" + pegAliasUSDCClassic
	depegUSDCUSDPair = "crypto:USDC/fiat:USD"
)

func depegServer(t *testing.T, usdcUSD string) string {
	t.Helper()
	usdc := installPegAliasRegistry(t)
	byPair := map[string]string{depegXLMUSDCPair: "0.10"}
	if usdcUSD != "" {
		byPair[depegUSDCUSDPair] = usdcUSD
	}
	srv := v1.New(v1.Options{
		PriceAt:           &recordingPriceAtReader{byPair: byPair},
		USDPeggedClassics: []canonical.Asset{usdc},
	})
	return startHTTPTest(t, srv.Handler()).URL
}

// TestPriceAt_ProxyDeviationBand: a triangulated fiat:USD answer served
// through a declared peg flags proxy_deviation only when the peg's own
// observed dollar price is more than 2% from $1.
func TestPriceAt_ProxyDeviationBand(t *testing.T) {
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name, usdcUSD string
		want          bool
	}{
		{"depegged below", "0.95", true},
		{"depegged above", "1.03", true},
		{"inside band", "1.019", false},
		{"exactly at band", "0.98", false},
		{"no observation", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := depegServer(t, tc.usdcUSD)
			env := getPegEnvelope(t, base+"/v1/price/at?asset=native&quote=fiat:USD&ts="+at)
			if !env.Flags.Triangulated {
				t.Fatal("flags.triangulated = false, want true (served through the peg)")
			}
			if env.Flags.ProxyDeviation != tc.want {
				t.Errorf("flags.proxy_deviation = %v, want %v", env.Flags.ProxyDeviation, tc.want)
			}
		})
	}
}

func TestPriceChanges_ProxyDeviationBand(t *testing.T) {
	for _, tc := range []struct {
		usdcUSD string
		want    bool
	}{{"0.95", true}, {"1.001", false}} {
		base := depegServer(t, tc.usdcUSD)
		resp := mustGet(t, base+"/v1/price/changes?asset=native&quote=fiat:USD")
		body, _ := readAll(resp)
		if got := strings.Contains(string(body), `"proxy_deviation":true`); got != tc.want {
			t.Errorf("usdc/usd=%s: proxy_deviation present = %v, want %v\n%s", tc.usdcUSD, got, tc.want, body)
		}
	}
}

// A declared peg that did not serve the answer still trips the flag: the
// proxy cannot say which peg a triangulated figure leans on, so any
// off-band declared peg is reported.
func TestPriceAt_ProxyDeviationAnyDeclaredPeg(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	pyusd := mustClassicAsset(t, "PYUSD", pegAliasPYUSDIssuer)
	srv := v1.New(v1.Options{
		PriceAt: &recordingPriceAtReader{byPair: map[string]string{
			depegXLMUSDCPair:        "0.10",
			depegUSDCUSDPair:        "1.00",
			"crypto:PYUSD/fiat:USD": "0.90",
		}},
		USDPeggedClassics: []canonical.Asset{usdc, pyusd},
	})
	base := startHTTPTest(t, srv.Handler()).URL
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	env := getPegEnvelope(t, base+"/v1/price/at?asset=native&quote=fiat:USD&ts="+at)
	if !env.Flags.ProxyDeviation {
		t.Error("flags.proxy_deviation = false, want true (PYUSD observed at 0.90)")
	}
}

func TestVWAP_ProxyDeviationBand(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	xlm, _ := canonical.ParseAsset("native")
	classicPair, _ := canonical.NewPair(xlm, usdc)
	trade := canonical.Trade{
		Source: "sdex", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Now().UTC().Add(-time.Minute),
		Pair:        classicPair,
		BaseAmount:  canonical.NewAmount(big.NewInt(100)),
		QuoteAmount: canonical.NewAmount(big.NewInt(16)),
	}
	for _, tc := range []struct {
		usdcUSD string
		want    bool
	}{{"0.95", true}, {"1.001", false}, {"", false}} {
		byPair := map[string]string{}
		if tc.usdcUSD != "" {
			byPair[depegUSDCUSDPair] = tc.usdcUSD
		}
		srv := v1.New(v1.Options{
			History: &pairAwareHistoryReader{tradesByPair: map[string][]canonical.Trade{
				depegXLMUSDCPair: {trade},
			}},
			PriceAt:           &recordingPriceAtReader{byPair: byPair},
			USDPeggedClassics: []canonical.Asset{usdc},
		})
		resp := mustGet(t, startHTTPTest(t, srv.Handler()).URL+"/v1/vwap?base=native&quote=fiat:USD")
		body, _ := readAll(resp)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"triangulated":true`) {
			t.Fatalf("usdc/usd=%q: status %d, body %s", tc.usdcUSD, resp.StatusCode, body)
		}
		if got := strings.Contains(string(body), `"proxy_deviation":true`); got != tc.want {
			t.Errorf("usdc/usd=%q: proxy_deviation present = %v, want %v\n%s", tc.usdcUSD, got, tc.want, body)
		}
	}
}
