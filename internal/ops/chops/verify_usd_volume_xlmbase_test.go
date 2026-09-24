// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"math/big"
	"strings"
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

// TestCheckXLMBaseBound_CEXScale — regression for the first live run of
// the bound (2026-08-04): base-leg scale is a CONNECTOR property, and
// off-chain CEX rows stamp 1e8 (not stroops). The un-fixed 1e7
// hardcode flagged every honest kraken XLM/EUR day at ratio ≈ 0.100.
func TestCheckXLMBaseBound_CEXScale(t *testing.T) {
	spec, err := timescale.NewUSDVolumeQuoteSpec(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rate := new(big.Rat).SetFloat64(0.16)
	// 10,000 XLM at CEX 1e8 scale = 1e12 raw. Honest usd_volume ≈
	// 10,000 × $0.16 = $1,600 (stored via the EUR→USD FX tier).
	g := timescale.TradeValuationGroup{
		Source:        "kraken",
		BaseAsset:     "crypto:XLM",
		QuoteAsset:    "fiat:EUR",
		PricedRows:    100,
		SumUSDVolume:  "1600.00",
		SumBaseAmount: "1000000000000",
	}
	if got := checkXLMBaseBound([]timescale.TradeValuationGroup{g}, spec, rate, 1, 20); got != 0 {
		t.Errorf("violations = %d, want 0 — honest CEX rows at 1e8 scale must pass", got)
	}
	// And a genuinely-wrong CEX group still fails.
	g.SumUSDVolume = "16000.00" // 10x over
	if got := checkXLMBaseBound([]timescale.TradeValuationGroup{g}, spec, rate, 1, 20); got != 1 {
		t.Errorf("violations = %d, want 1 — a 10x-over CEX group must still fail", got)
	}
}

// TestCheckXLMBaseBound_SubCentDust — #372's residual: after the restamp
// every remaining violation had stored=0.00 and expected≈0.00, ratios that
// are sub-cent quantisation rather than valuation error. Those are exempt;
// a breach with a cent on EITHER side must still count at any group size.
func TestCheckXLMBaseBound_SubCentDust(t *testing.T) {
	spec, err := timescale.NewUSDVolumeQuoteSpec(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rate := new(big.Rat).SetFloat64(0.16)
	group := func(baseStroops, sumUSD string) timescale.TradeValuationGroup {
		return timescale.TradeValuationGroup{
			Source:        "sdex",
			BaseAsset:     "native",
			QuoteAsset:    "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V",
			PricedRows:    3,
			SumUSDVolume:  sumUSD,
			SumBaseAmount: baseStroops,
		}
	}

	cases := []struct {
		name        string
		baseStroops string
		sumUSD      string
		want        int
	}{
		// 100 stroops × $0.16 → expected $0.0000016; stored 2× that.
		{"both sides round to $0.00 exempt", "100", "0.0000032", 0},
		// 300,000 stroops → expected $0.0048; stored $0.0032 (ratio 0.667).
		{"just under half a cent both sides exempt", "300000", "0.0032", 0},
		// Same dust expected, stored $5: an overvaluation, not quantisation.
		{"dust expected but stored has cents caught", "100", "5.00", 1},
		// 500,000 stroops → expected $0.008 (renders $0.01); stored half.
		{"expected rounds to a cent caught", "500000", "0.004", 1},
		// $160 expected stored at $0.0002: a 977,000x-class undervaluation.
		{"real group valued near zero caught", "10000000000", "0.0002", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := group(tc.baseStroops, tc.sumUSD)
			if got := checkXLMBaseBound([]timescale.TradeValuationGroup{g}, spec, rate, 1, 20); got != tc.want {
				t.Errorf("violations = %d, want %d", got, tc.want)
			}
		})
	}

	if footer := usdVolumeFooterText(0); !strings.Contains(footer, "both round to $0.00 is printed, not counted") {
		t.Errorf("footer does not state the sub-cent exemption:\n%s", footer)
	}
}
