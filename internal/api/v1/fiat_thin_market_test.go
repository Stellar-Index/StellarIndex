// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// deepOnlyStore measures every market thin, as an empty one is, except the listed deep pairs.
type deepOnlyStore struct{ deep map[string]bool }

func (s deepOnlyStore) measure(bases, quotes []canonical.Asset) timescale.MarketSubstance {
	for _, b := range bases {
		for _, q := range quotes {
			if s.deep[b.String()+"/"+q.String()] {
				return thickMarket
			}
		}
	}
	return thinMarket
}

func (s deepOnlyStore) PairMarketSubstance(_ context.Context, bases, quotes []canonical.Asset, _ time.Duration) (timescale.MarketSubstance, error) {
	return s.measure(bases, quotes), nil
}

func (s deepOnlyStore) PairMarketSubstanceAt(_ context.Context, bases, quotes []canonical.Asset, _ time.Time, _ time.Duration, _ timescale.HistoryGranularity) (timescale.MarketSubstance, error) {
	return s.measure(bases, quotes), nil
}

// A fiat-quoted raw-trade answer is combined from the USD-peg books, so its thin_market verdict must be measured over those books,
// not over the literal X/fiat:USD market that only a CEX feed would populate.
func TestFiatQuotedThinMarketMeasuresTheConstituentBooks(t *testing.T) {
	aqua := mustParseAsset(t, aquaClassicID)
	usdcClassic := mustParseAsset(t, usdcClassicID)
	bookPair, err := canonical.NewPair(aqua, usdcClassic)
	if err != nil {
		t.Fatalf("NewPair book: %v", err)
	}
	t0 := fiatParityBucketStart()
	reader := &fiatConstituentReader{tradesByPair: map[string][]canonical.Trade{
		fiatParityPairKey(bookPair): {fiatParityTrade(bookPair, 1, t0.Add(90*time.Second), 1000_0000000, 200_0000000)},
	}}
	win := "&from=" + t0.Format(time.RFC3339) + "&to=" + t0.Add(time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name     string
		deep     map[string]bool
		wantThin bool
	}{
		{"deep book, empty literal pair", map[string]bool{fiatParityPairKey(bookPair): true}, false},
		{"thin book, empty literal pair", nil, true},
	} {
		gate := pricingguard.NewSubstanceGate(deepOnlyStore{deep: tc.deep}, pricingguard.SubstanceGateOptions{})
		ts := httpTestServer(t, v1.New(v1.Options{
			History:           reader,
			Substance:         gate,
			USDPeggedClassics: []canonical.Asset{usdcClassic},
		}))
		for _, path := range []string{"/v1/ohlc", "/v1/vwap", "/v1/twap"} {
			status, body := fetch(t, ts.URL+path+"?base="+aquaClassicID+"&quote=fiat:USD&outlier_sigma=0"+win)
			if status != http.StatusOK {
				t.Fatalf("%s %s: status %d: %s", tc.name, path, status, body)
			}
			flags, _ := decodeMap(t, body)["flags"].(map[string]any)
			if got := flags["thin_market"] == true; got != tc.wantThin {
				t.Errorf("%s %s: thin_market = %v, want %v", tc.name, path, got, tc.wantThin)
			}
		}
	}
}
