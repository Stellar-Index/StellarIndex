// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The base-anchored tier reads the SAME resolver rate the quote-side FX
// tier reads, and for a non-XLM token that rate is usually tier 3b's
// <token>/XLM x XLM/USD bridge — writable by anyone who pays
// bridgeLegMinUSDVolume. The FX tier bounds a value resting on such a
// rate ([boundUSDVolume]); until F044 / K045 the base anchor stored it
// verbatim, so the $182M fake-print class stayed open through the base
// leg: plant TOKEN_A/XLM, then swap base=TOKEN_A against a never-priced
// TOKEN_B so the quote tier declines and the anchor fires.
//
// Every case below runs through [tradeUSDVolume], the function the live
// insert path calls, not through the helper — the defect was a tier the
// waterfall reached unguarded, so the waterfall is what is asserted.

// boundIssuer issues both test tokens. They are classic so
// [baseAnchorEligible] admits the base at the 1e7 scale.
const boundIssuer = "GBGRBCUB6L7LH4JQ6EPDP7REH2DDACMCUQI76M3P6DM52QWU2Z5LIEVW"

// boundAnchorTrade is base/TOKB on sdex. TOKB never has a rate in any
// resolver below unless a case adds one, which is what makes the quote
// tier decline and the anchor fire.
func boundAnchorTrade(t *testing.T, base canonical.Asset, baseStroops *big.Int) canonical.Trade {
	t.Helper()
	quote, err := canonical.NewClassicAsset("TOKB", boundIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset TOKB: %v", err)
	}
	return canonical.Trade{
		Source:      "sdex",
		Ledger:      63_890_200,
		TxHash:      "f044",
		OpIndex:     0,
		Timestamp:   time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		Pair:        canonical.Pair{Base: base, Quote: quote},
		BaseAmount:  canonical.NewAmount(baseStroops),
		QuoteAmount: canonical.NewAmount(big.NewInt(1_000_000_000)),
	}
}

// boundTokenA is the bridge-writable base.
func boundTokenA(t *testing.T) canonical.Asset {
	t.Helper()
	a, err := canonical.NewClassicAsset("TOKA", boundIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset TOKA: %v", err)
	}
	return a
}

func boundQuoteSpec(t *testing.T) *USDVolumeQuoteSpec {
	t.Helper()
	spec, err := NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}, nil)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}
	return spec
}

// TestTradeUSDVolume_BaseAnchorRefusesAnUncrossCheckablePrintAboveTheCeiling
// is the attack: 20,000 TOKEN_A (2e11 stroops) x a planted $50,000 rate
// is $1,000,000,000 resting on one attacker-authored leg. It must be
// refused (usd_volume NULL), exactly as the quote side refuses it.
func TestTradeUSDVolume_BaseAnchorRefusesAnUncrossCheckablePrintAboveTheCeiling(t *testing.T) {
	t.Parallel()
	tokenA := boundTokenA(t)
	tr := boundAnchorTrade(t, tokenA, big.NewInt(200_000_000_000))
	poisoned := stubFXResolver{prices: map[string]string{tokenA.String(): "50000"}}

	if got := tradeUSDVolume(context.Background(), tr, boundQuoteSpec(t), poisoned); got != nil {
		t.Fatalf("single-leg base-anchored print above singleLegMaxUSDVolume: want NULL (refused), got %q", *got)
	}
}

// TestTradeUSDVolume_BaseAnchorBoundLeavesPlausiblePrintsByteIdentical
// is the other half of a lossy guard: the refusal must fire ONLY above
// the ceiling. The token/token class this tier exists for (99.2% of the
// remaining unpriced trades, 2026-07-22) keeps its value to the digit,
// and the boundary itself is inclusive, as it is on the quote side
// (`> ceiling` refuses; `== ceiling` serves).
func TestTradeUSDVolume_BaseAnchorBoundLeavesPlausiblePrintsByteIdentical(t *testing.T) {
	t.Parallel()
	tokenA := boundTokenA(t)
	cases := []struct {
		name string
		rate string
		want string
	}{
		{"ordinary", "2.50", "50000.00000000"},
		{"exactly at the ceiling", "5000", "100000000.00000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := boundAnchorTrade(t, tokenA, big.NewInt(200_000_000_000))
			fx := stubFXResolver{prices: map[string]string{tokenA.String(): tc.rate}}
			got := tradeUSDVolume(context.Background(), tr, boundQuoteSpec(t), fx)
			if got == nil {
				t.Fatalf("plausible base-anchored print: want %s, got NULL", tc.want)
			}
			if *got != tc.want {
				t.Fatalf("usd_volume = %q, want %q", *got, tc.want)
			}
		})
	}
}

// TestTradeUSDVolume_BaseAnchorRefusesJustAboveTheCeiling pins the edge
// from the other side: 20,000 x $5000.00000001 = $100,000,000.0002.
func TestTradeUSDVolume_BaseAnchorRefusesJustAboveTheCeiling(t *testing.T) {
	t.Parallel()
	tokenA := boundTokenA(t)
	tr := boundAnchorTrade(t, tokenA, big.NewInt(200_000_000_000))
	fx := stubFXResolver{prices: map[string]string{tokenA.String(): "5000.00000001"}}
	if got := tradeUSDVolume(context.Background(), tr, boundQuoteSpec(t), fx); got != nil {
		t.Fatalf("just above the ceiling: want NULL, got %q", *got)
	}
}

// TestTradeUSDVolume_XLMAnchorIsExemptFromTheBound arms the trigger on
// the one anchor that must NOT be bounded. XLM is the bridge's own
// anchor: its rate is a direct XLM/USD market nobody can author, and
// base_amount is XLM that actually moved, so the value is exact rather
// than estimated. A ceiling would NULL a real (if enormous) trade, and a
// cross-check would let a planted token rate drag an exact value DOWN —
// see [tradeUSDVolumeViaXLMQuoteAnchorFor]. 1e9 XLM x $0.50 = $500M.
func TestTradeUSDVolume_XLMAnchorIsExemptFromTheBound(t *testing.T) {
	t.Parallel()
	stroops, ok := new(big.Int).SetString("10000000000000000", 10) // 1e9 XLM
	if !ok {
		t.Fatal("bad stroop literal")
	}
	tr := boundAnchorTrade(t, canonical.NativeAsset(), stroops)
	// The quote token carries a planted rate that values the trade at
	// one cent — far more than 10x below the XLM leg. It must not win.
	fx := stubFXResolver{prices: map[string]string{
		canonical.NativeAsset().String(): "0.5",
		tr.Pair.Quote.String():           "0.0001",
	}}
	got := tradeUSDVolume(context.Background(), tr, boundQuoteSpec(t), fx)
	if got == nil {
		t.Fatal("XLM-anchored trade above the single-leg ceiling: want its exact value, got NULL")
	}
	if want := "500000000.00000000"; *got != want {
		t.Fatalf("usd_volume = %q, want %q", *got, want)
	}
}

// TestRestampAnchorInheritsTheBound pins the shared primitive. The
// restamp tiers reach the anchor through
// [tradeUSDVolumeViaXLMBaseAnchorFor] rather than the waterfall, so a
// bound applied at the waterfall's call sites would have left a backfill
// free to re-write the very rows the live path now refuses.
func TestRestampAnchorInheritsTheBound(t *testing.T) {
	t.Parallel()
	tokenA := boundTokenA(t)
	tr := boundAnchorTrade(t, tokenA, big.NewInt(200_000_000_000))
	poisoned := stubFXResolver{prices: map[string]string{tokenA.String(): "50000"}}

	if got := tradeUSDVolumeViaXLMBaseAnchorFor(context.Background(), tr, poisoned); got != nil {
		t.Fatalf("restamp entry point, above-ceiling non-XLM anchor: want NULL, got %q", *got)
	}
}
