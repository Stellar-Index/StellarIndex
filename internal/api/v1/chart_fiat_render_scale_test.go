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
		{Bucket: d1, RateUSD: 2.5e12, InverseUSD: 4e-13},
		{Bucket: d2, RateUSD: 7.18, InverseUSD: 1 / 7.18},
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
