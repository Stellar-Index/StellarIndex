// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestOHLCSingleBar_MicroPriceKeepsItsDigits: a token trading at 1.99e-10 of its quote must be served at that
// price, as /v1/price's aggregator string is, not rounded to one significant digit ("0.0000000001", 50% low).
func TestOHLCSingleBar_MicroPriceKeepsItsDigits(t *testing.T) {
	usdc := mustParseAsset(t, usdcClassicID)
	pair, _ := canonical.NewPair(mustParseAsset(t, "native"), usdc)
	t0 := fiatParityBucketStart()
	reader := &fiatConstituentReader{tradesByPair: map[string][]canonical.Trade{
		fiatParityPairKey(pair): {fiatParityTrade(pair, 1, t0.Add(time.Minute), 1_000_000_000_000, 199)},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader}))

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote="+usdcClassicID+"&outlier_sigma=0"+fiatParityWindow())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.OHLCBar `json:"data"`
	}
	mustDecode(t, resp, &env)
	want := big.NewRat(199, 1_000_000_000_000)
	for name, got := range map[string]string{"open": env.Data.Open, "high": env.Data.High, "low": env.Data.Low, "close": env.Data.Close} {
		if mustRat(t, got).Cmp(want) != 0 {
			t.Errorf("%s = %q, want 1.99e-10 exactly", name, got)
		}
	}
}
