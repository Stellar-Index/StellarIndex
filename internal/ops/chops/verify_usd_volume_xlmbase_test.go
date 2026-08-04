// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The XLM-BASE BOUND closes the class that shipped invisible for 13
// days: estimated-tier rows whose base is XLM have a checkable anchor
// (Σusd_volume ≈ Σbase/1e7 × XLM/USD), and the 2026-08-04 poisoning
// was 10×–10⁶× outside any honest tolerance.
func TestCheckXLMBaseBound(t *testing.T) {
	spec, err := timescale.NewUSDVolumeQuoteSpec(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rate := new(big.Rat).SetFloat64(0.16) // day VWAP $0.16/XLM

	// 1,000 XLM base (1e10 stroops) → expected ≈ $160.
	group := func(sumUSD string) timescale.TradeValuationGroup {
		return timescale.TradeValuationGroup{
			Source:    "sdex",
			BaseAsset: "native",
			// On-chain quote with NO peg on the (empty) spec →
			// TierEstimated — the exact shape of the poisoned rows.
			QuoteAsset:    "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V",
			PricedRows:    10,
			SumUSDVolume:  sumUSD,
			SumBaseAmount: "10000000000",
		}
	}

	cases := []struct {
		name   string
		sumUSD string
		want   int
	}{
		{"honest valuation passes", "160.00", 0},
		{"within 30 percent passes", "130.00", 0},
		{"incident-shaped 8.5M overvaluation caught", "8559224.00", 1},
		{"977000x undervaluation caught", "0.000175", 1},
		{"just outside the band caught", "300.00", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkXLMBaseBound([]timescale.TradeValuationGroup{group(tc.sumUSD)}, spec, rate, 1, 20)
			if got != tc.want {
				t.Errorf("violations = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("non-XLM base is out of scope", func(t *testing.T) {
		g := group("999999")
		g.BaseAsset = "AQUA-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
		if got := checkXLMBaseBound([]timescale.TradeValuationGroup{g}, spec, rate, 1, 20); got != 0 {
			t.Errorf("violations = %d, want 0 for non-XLM base", got)
		}
	})

	t.Run("pegged tier is out of scope (judged exactly elsewhere)", func(t *testing.T) {
		pegged, err := timescale.NewUSDVolumeQuoteSpec(
			[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		g := group("8559224.00")
		g.QuoteAsset = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		if got := checkXLMBaseBound([]timescale.TradeValuationGroup{g}, pegged, rate, 1, 20); got != 0 {
			t.Errorf("violations = %d, want 0 — quote-pegged groups belong to the exact check", got)
		}
	})

	t.Run("below min-rows skipped", func(t *testing.T) {
		g := group("8559224.00")
		g.PricedRows = 2
		if got := checkXLMBaseBound([]timescale.TradeValuationGroup{g}, spec, rate, 5, 20); got != 0 {
			t.Errorf("violations = %d, want 0 below min-rows", got)
		}
	})
}
