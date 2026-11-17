// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// thinSeriesReader serves trades per pair (single bar) and one bar for any pair (series).
type thinSeriesReader struct {
	pairAwareHistoryReader
}

func (r *thinSeriesReader) OHLCSeries(
	_ context.Context, _ canonical.Pair, _ string, from, _ time.Time, _ int,
) ([]v1.OHLCSeriesBar, error) {
	return []v1.OHLCSeriesBar{{
		T: v1.WireTime(from.Truncate(time.Hour).Add(time.Hour)), O: "1", H: "1", L: "1", C: "1",
		VBase: "50000000", VQuote: "50000000", N: 5, Sources: []string{"sdex"},
	}}, nil
}

// TestOHLCFlagsThinMarketWithEvidence: /v1/ohlc serves a sub-floor market (raw tier) in both modes, and must
// say so as /v1/vwap and /v1/twap do: flags.thin_market plus the substance block.
func TestOHLCFlagsThinMarketWithEvidence(t *testing.T) {
	native := canonical.NativeAsset()
	thin, err := canonical.ParseAsset(thinAssetID)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := canonical.ParseAsset(clearedAssetID)
	if err != nil {
		t.Fatal(err)
	}
	reader := &thinSeriesReader{pairAwareHistoryReader{tradesByPair: map[string][]canonical.Trade{
		thin.String() + "/native":    {scamTestTrade(t, thin, native)},
		cleared.String() + "/native": {scamTestTrade(t, cleared, native)},
	}}}
	gate := pricingguard.NewSubstanceGate(thinStore{thin: map[string]bool{thinAssetID: true}}, pricingguard.SubstanceGateOptions{})
	ts := startHTTPTest(t, v1.New(v1.Options{History: reader, Substance: gate}).Handler())

	now := time.Now().UTC().Truncate(time.Hour)
	series := "&interval=1h&from=" + now.Add(-6*time.Hour).Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339)
	for _, mode := range []string{"", series} {
		status, body := fetch(t, ts.URL+"/v1/ohlc?base="+thinAssetID+"&quote=native"+mode)
		if status != http.StatusOK {
			t.Fatalf("ohlc%s thin: status %d, want 200 (raw tier serves): %s", mode, status, body)
		}
		env := decodeMap(t, body)
		if flags, _ := env["flags"].(map[string]any); flags["thin_market"] != true {
			t.Errorf("ohlc%s thin: flags.thin_market = %v, want true", mode, flags["thin_market"])
		}
		data, _ := env["data"].(map[string]any)
		assertEvidence(t, data["substance"], thinAssetID)

		status, body = fetch(t, ts.URL+"/v1/ohlc?base="+clearedAssetID+"&quote=native"+mode)
		if status != http.StatusOK {
			t.Fatalf("ohlc%s cleared: status %d, want 200: %s", mode, status, body)
		}
		if strings.Contains(string(body), "thin_market") || strings.Contains(string(body), "substance") {
			t.Errorf("ohlc%s cleared pair carries a thin marker: %s", mode, body)
		}
	}
}
