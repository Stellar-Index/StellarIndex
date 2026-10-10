// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
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

// TestPriceChanges_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn is the same
// guard on /v1/price/changes.
func TestPriceChanges_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn(t *testing.T) {
	base, reader := pegPriceAtServer(t)

	resp := mustGet(t, base+"/v1/price/changes?asset="+pegAliasUSDCSAC+"&quote=fiat:USD")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	for _, want := range []string{`"current_price":"1.0004"`, `"quote":"fiat:USD"`, `"triangulated":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\n%s", want, body)
		}
	}
	assertSecondPegWalked(t, reader)
}
