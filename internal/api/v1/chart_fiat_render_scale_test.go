// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// The direct fiat chart leg (one USD-quoted series, no cross) renders
// through the same magnitude-relative decimal renderer as every other
// price surface: a hyperinflation-shaped rate below 1e-10 USD must not
// collapse to "0.0000000000", and a normal rate keeps its ten places.
func TestChart_Fiat_DirectLeg_TinyRateSurvives(t *testing.T) {
	d1 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: d1, RateUSDText: "2500000000000", InverseUSDText: "0.0000000000004"},
		{Bucket: d2, RateUSDText: "7.18", InverseUSDText: "0.13927576601671309192"},
	}}
	srv := v1.New(v1.Options{History: &stubHistoryReader{}, FXHistory: fx})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/chart?asset=fiat:CNY&quote=fiat:USD&timeframe=1y&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.ChartSeries `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := len(env.Data.Points); got != 2 {
		t.Fatalf("got %d points, want 2", got)
	}
	if got, want := env.Data.Points[0].P, "0.0000000000004000000000000"; got != want {
		t.Errorf("tiny inverse rate P = %q, want %q", got, want)
	}
	if back, ok := new(big.Rat).SetString(env.Data.Points[0].P); !ok || back.Sign() <= 0 {
		t.Errorf("tiny inverse rate %q reparses non-positive", env.Data.Points[0].P)
	}
	if got, want := env.Data.Points[1].P, "0.1392757660"; got != want {
		t.Errorf("normal inverse rate P = %q, want %q", got, want)
	}
}

// Every fiat chart leg serves the fx_quotes NUMERIC text exactly. A rate
// of 2^53+1 has no float64 representation; a float hop renders it as
// ...992 or ...994 while claiming ten exact decimal places.
func TestChart_Fiat_RatesAbove2to53StayExact(t *testing.T) {
	d1 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	const huge = "9007199254740993" // 2^53 + 1
	const want = huge + ".0000000000"
	fx := &tickerFXHistoryReader{byTicker: map[string][]v1.FXQuotePoint{
		"EUR": {{Bucket: d1, RateUSDText: "1", InverseUSDText: "1"}},
		"JPY": {{Bucket: d1, RateUSDText: huge, InverseUSDText: huge}},
	}}
	srv := v1.New(v1.Options{History: &stubHistoryReader{}, FXHistory: fx})
	ts := httpTestServer(t, srv)
	for _, q := range []string{
		"asset=fiat:USD&quote=fiat:JPY", // rate_usd leg
		"asset=fiat:JPY&quote=fiat:USD", // inverse_usd leg
		"asset=fiat:EUR&quote=fiat:JPY", // cross: rate_usd[JPY] / rate_usd[EUR]
	} {
		resp := mustGet(t, ts.URL+"/v1/chart?"+q+"&timeframe=1y&granularity=1d")
		var env struct {
			Data v1.ChartSeries `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("%s: decode: %v", q, err)
		}
		if resp.StatusCode != http.StatusOK || len(env.Data.Points) != 1 {
			t.Fatalf("%s: status=%d points=%d, want 200 and 1", q, resp.StatusCode, len(env.Data.Points))
		}
		if got := env.Data.Points[0].P; got != want {
			t.Errorf("%s: P = %q, want %q", q, got, want)
		}
	}
}
