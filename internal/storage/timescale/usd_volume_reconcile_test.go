package timescale

import (
	"context"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── C4-055/066: the exact-tier valuation identity ───────────────

func testQuoteSpec(t *testing.T) *USDVolumeQuoteSpec {
	t.Helper()
	// TWO declared pegs, so a both-legs-pegged pair is constructible. That
	// pair is the only one whose answer depends on the ORDER the waterfall
	// probes its legs in — see TestClassifyUSDVolumeTier_TracksTheWaterfall.
	spec, err := NewUSDVolumeQuoteSpec(
		[]string{
			"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			"USDX-GBUYUAI75XXWDZEKLY66CFYKQPET5JR4EENXZBUZ3YXZ7DS56Z4OKOFU",
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}
	return spec
}

// TestClassifyUSDVolumeTier pins that the tier a check attributes to a
// (source, base, quote) group is the tier the INSERT path would actually
// take — including the order of the two pegged-leg probes, since tier 2b is
// only reachable once the quote leg has declined.
func TestClassifyUSDVolumeTier(t *testing.T) {
	spec := testQuoteSpec(t)
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

	cases := []struct {
		name         string
		source       string
		base, quote  string
		wantTier     USDVolumeTier
		wantDecimals int
	}{
		// Off-chain: fiat:USD quote → tier 1, at the SOURCE's registered
		// scale — 8 for CEXes, 6 for the FX pollers (CS-040: the checker
		// must divide by the same per-source scale the stamper uses, or
		// it certifies a 100× error as correct).
		{"cex USD quote", "binance", "crypto:XLM", "fiat:USD", TierQuotePegged, 8},
		{"fx USD quote", "polygon-forex", "fiat:EUR", "fiat:USD", TierQuotePegged, 6},
		{"fx USD base", "exchangeratesapi", "fiat:USD", "fiat:JPY", TierBasePegged, 6},
		// Off-chain: neither leg USD → estimated (FX tier).
		{"cex EUR quote", "binance", "crypto:XLM", "fiat:EUR", TierEstimated, 0},
		// On-chain: declared classic USD peg on the quote leg → tier 2,
		// classic credits are uniformly 7-decimal.
		{"dex pegged quote", "sdex", "native", usdc, TierQuotePegged, 7},
		// On-chain: the dollar leg is the BASE — tier 2b, the case a
		// quote-only waterfall used to miss entirely.
		{"dex pegged base", "sdex", usdc, "native", TierBasePegged, 7},
		// On-chain: neither leg pegged → estimated.
		{"dex unpegged", "sdex", "native", "crypto:BTC", TierEstimated, 0},
		// Non-exchange source class: tradeUSDVolume returns nil for these
		// by construction, so any value on such a row came from elsewhere.
		{"oracle source", "reflector-dex", "native", "fiat:USD", TierUnvaluable, 0},
		// An UNREGISTERED source: external.Lookup defaults it to
		// ClassExchange with an EMPTY subclass, so usdVolumeDecimals
		// declines both legs and the insert path falls through to the FX
		// tiers. The classifier must mirror that rather than "helpfully"
		// calling it unvaluable — the two must agree about what the writer
		// would have done, including in the odd corners.
		{"unregistered source", "some-new-venue", "native", "fiat:USD", TierEstimated, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tier, decimals, err := ClassifyUSDVolumeTier(tc.source, tc.base, tc.quote, spec)
			if err != nil {
				t.Fatalf("ClassifyUSDVolumeTier: %v", err)
			}
			if tier != tc.wantTier {
				t.Errorf("tier = %q, want %q", tier, tc.wantTier)
			}
			if decimals != tc.wantDecimals {
				t.Errorf("decimals = %d, want %d", decimals, tc.wantDecimals)
			}
		})
	}
}

// TestClassifyUSDVolumeTier_UnparseableAssetReported — an asset id on a
// LANDED trade that the canonical parser rejects is its own finding. It must
// surface as an error, not be silently folded into the estimated bucket.
func TestClassifyUSDVolumeTier_UnparseableAssetReported(t *testing.T) {
	if _, _, err := ClassifyUSDVolumeTier("binance", "!!not-an-asset!!", "fiat:USD", testQuoteSpec(t)); err == nil {
		t.Error("an unparseable base asset must be reported, not silently classified")
	}
}

// TestUSDVolumeTier_Exact — only the pegged tiers carry an identity that can
// be judged without a calibrated tolerance. If this ever reports true for
// the estimated tier, the command would start failing on legitimate FX
// error.
func TestUSDVolumeTier_Exact(t *testing.T) {
	for tier, want := range map[USDVolumeTier]bool{
		TierQuotePegged: true,
		TierBasePegged:  true,
		TierEstimated:   false,
		TierUnvaluable:  false,
	} {
		if got := tier.Exact(); got != want {
			t.Errorf("%q.Exact() = %v, want %v", tier, got, want)
		}
	}
}

// TestExactTierDelta_HoldsAndCatchesOneUnit is the C4-055/066 core.
//
// For an exact tier the stored column must satisfy
// usd_volume == pegged_leg / 10^decimals with NO tolerance — it is a decimal
// rescaling of a number already on the row, not a price lookup. This pins
// both directions: a correct group nets to exactly zero, and a group off by
// ONE unit at the rendering scale (1e-8 USD) is caught rather than lost in
// float slop.
func TestExactTierDelta_HoldsAndCatchesOneUnit(t *testing.T) {
	// 3 on-chain trades, 7-decimal pegged quote leg. Σquote = 4,500,000,000
	// stroops → $450 exactly.
	g := TradeValuationGroup{
		Source: "sdex", BaseAsset: "native",
		QuoteAsset:     "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		PricedRows:     3,
		SumUSDVolume:   "450.00000000",
		SumBaseAmount:  "9000000000",
		SumQuoteAmount: "4500000000",
	}

	delta, ok := ExactTierDelta(g, TierQuotePegged, 7)
	if !ok {
		t.Fatal("ExactTierDelta: sums failed to parse")
	}
	if delta.Sign() != 0 {
		t.Errorf("a correctly-valued group has delta %s, want exactly 0", delta.FloatString(12))
	}

	// One unit off at the render scale: $450.00000001. At a REALISTIC daily
	// volume this magnitude falls below the float64 ulp entirely — see the
	// DB-backed sibling (test/integration/usd_volume_value_reconcile_test.go),
	// which sizes the fixture to $500M and asserts a naive float check reports
	// zero. That is why this comparison is exact rational arithmetic.
	g.SumUSDVolume = "450.00000001"
	delta, ok = ExactTierDelta(g, TierQuotePegged, 7)
	if !ok {
		t.Fatal("ExactTierDelta: sums failed to parse")
	}
	want := new(big.Rat).SetFrac(big.NewInt(1), big.NewInt(100_000_000))
	if delta.Cmp(want) != 0 {
		t.Errorf("delta = %s, want %s (one unit at the 8-decimal render scale)",
			delta.FloatString(12), want.FloatString(12))
	}
	// And it must exceed the rounding slack, i.e. be judged a violation.
	if slack := USDVolumeRoundingSlack(7, g.PricedRows); new(big.Rat).Abs(delta).Cmp(slack) <= 0 {
		t.Errorf("a one-unit error (%s) was absorbed by slack %s — the exact tier would not fail",
			delta.FloatString(12), slack.FloatString(12))
	}
}

// TestExactTierDelta_BasePeggedUsesTheBaseLeg — tier 2b rescales the BASE
// amount. Reading the quote leg here would make every USDC/XLM-oriented
// market look catastrophically wrong (and, worse, could make a genuinely
// wrong one look right).
func TestExactTierDelta_BasePeggedUsesTheBaseLeg(t *testing.T) {
	g := TradeValuationGroup{
		PricedRows:     2,
		SumUSDVolume:   "12.00000000",
		SumBaseAmount:  "120000000", // /1e7 = 12
		SumQuoteAmount: "999999999", // the leg that did NOT price this group
	}
	delta, ok := ExactTierDelta(g, TierBasePegged, 7)
	if !ok {
		t.Fatal("ExactTierDelta: sums failed to parse")
	}
	if delta.Sign() != 0 {
		t.Errorf("base-pegged delta = %s, want 0 — the base leg is the one that priced it", delta.FloatString(12))
	}
}

// TestExactTierDelta_RefusesInexactTiers — the estimated tiers have no
// reproducible identity, so there is nothing to subtract. Returning a
// plausible-looking zero here would silently "verify" the half of the column
// this check deliberately does not judge.
func TestExactTierDelta_RefusesInexactTiers(t *testing.T) {
	g := TradeValuationGroup{SumUSDVolume: "1", SumBaseAmount: "1", SumQuoteAmount: "1"}
	for _, tier := range []USDVolumeTier{TierEstimated, TierUnvaluable} {
		if _, ok := ExactTierDelta(g, tier, 7); ok {
			t.Errorf("ExactTierDelta reported a delta for the %q tier", tier)
		}
	}
}

// TestUSDVolumeRoundingSlack — the slack is DERIVED arithmetic (the worst
// case of a known rounding rule), never a calibrated threshold. At the
// decimal scales the peg spec actually produces today (7 on-chain, 8
// off-chain) the render is lossless, so the slack is exactly ZERO and the
// identity is checkable with no tolerance whatsoever.
func TestUSDVolumeRoundingSlack(t *testing.T) {
	for _, decimals := range []int{0, 7, 8} {
		if s := USDVolumeRoundingSlack(decimals, 1_000_000); s.Sign() != 0 {
			t.Errorf("slack at %d decimals = %s, want 0 (FloatString(8) is lossless there)",
				decimals, s.FloatString(12))
		}
	}
	// Only a hypothetical >8-decimal peg introduces rounding, bounded at
	// half an ulp per row.
	got := USDVolumeRoundingSlack(18, 4)
	want := new(big.Rat).SetFrac(big.NewInt(4), big.NewInt(200_000_000)) // 4 × 0.5e-8
	if got.Cmp(want) != 0 {
		t.Errorf("slack at 18 decimals over 4 rows = %s, want %s", got.FloatString(12), want.FloatString(12))
	}
	if s := USDVolumeRoundingSlack(18, 0); s.Sign() != 0 {
		t.Errorf("slack over zero rows = %s, want 0", s.FloatString(12))
	}
}

// TestPeggedLegSum_PicksTheLegThatPriced — a defensive pin on the mapping
// ExactTierDelta depends on.
func TestPeggedLegSum_PicksTheLegThatPriced(t *testing.T) {
	g := TradeValuationGroup{SumBaseAmount: "BASE", SumQuoteAmount: "QUOTE"}
	if got := g.PeggedLegSum(TierQuotePegged); got != "QUOTE" {
		t.Errorf("quote-pegged leg = %q, want QUOTE", got)
	}
	if got := g.PeggedLegSum(TierBasePegged); got != "BASE" {
		t.Errorf("base-pegged leg = %q, want BASE", got)
	}
	if got := g.PeggedLegSum(TierEstimated); got != "" {
		t.Errorf("estimated tier leg = %q, want empty", got)
	}
}

// TestClassifyUSDVolumeTier_TracksTheWaterfall is the lockstep guard, and it
// is the one that actually matters.
//
// TestClassifyUSDVolumeTier above pins hardcoded expectations, so it stays
// green through a change that re-orders the waterfall's legs or swaps which
// amount a tier divides — the classifier would then attribute the wrong tier
// and verify-usd-volume would check `usd_volume == quote/10^d` on rows the
// writer built from the BASE leg, reporting a fleet-wide violation (or, in
// the mirror case, silently verifying nothing).
//
// This closes that by round-tripping a real canonical.Trade through the
// PRODUCTION valuation function and asserting the classifier's answer
// reproduces it exactly: same tier semantics, same decimal scale, same leg.
// If tradeUSDVolume changes and ClassifyUSDVolumeTier does not, this fails.
func TestClassifyUSDVolumeTier_TracksTheWaterfall(t *testing.T) {
	spec := testQuoteSpec(t)
	usdcAsset, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usdx, err := canonical.NewClassicAsset("USDX", "GBUYUAI75XXWDZEKLY66CFYKQPET5JR4EENXZBUZ3YXZ7DS56Z4OKOFU")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		source      string
		base, quote canonical.Asset
		baseAmt     int64
		quoteAmt    int64
	}{
		{"cex USD quote (tier 1/2)", "binance", xlm, usd, 10_000_000_000, 1_250_000_000},
		{"dex pegged quote (tier 2)", "sdex", canonical.NativeAsset(), usdcAsset, 10_000_000_000, 1_250_000_000},
		{"dex pegged base (tier 2b)", "sdex", usdcAsset, canonical.NativeAsset(), 1_250_000_000, 10_000_000_000},
		// BOTH legs pegged — the only shape whose answer depends on the
		// ORDER the waterfall probes its legs in, and therefore the only
		// one that catches a re-ordering. The amounts differ so picking the
		// wrong leg produces a different number rather than the same one.
		{"dex both legs pegged (quote wins)", "sdex", usdcAsset, usdx, 1_250_000_000, 9_990_000_000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pair, perr := canonical.NewPair(tc.base, tc.quote)
			if perr != nil {
				t.Fatal(perr)
			}
			tr := canonical.Trade{
				Source:      tc.source,
				Pair:        pair,
				BaseAmount:  canonical.NewAmount(big.NewInt(tc.baseAmt)),
				QuoteAmount: canonical.NewAmount(big.NewInt(tc.quoteAmt)),
			}

			// The production waterfall's answer.
			got := tradeUSDVolume(context.Background(), tr, spec, nil)
			if got == nil {
				t.Fatalf("tradeUSDVolume declined a fixture the classifier calls an exact tier")
			}

			// The classifier's answer, reconstructed from the same row.
			tier, decimals, cerr := ClassifyUSDVolumeTier(tc.source, tc.base.String(), tc.quote.String(), spec)
			if cerr != nil {
				t.Fatalf("ClassifyUSDVolumeTier: %v", cerr)
			}
			if !tier.Exact() {
				t.Fatalf("tier = %q, want an exact tier for this fixture", tier)
			}

			leg := tc.quoteAmt
			if tier == TierBasePegged {
				leg = tc.baseAmt
			}
			want := new(big.Rat).SetFrac(big.NewInt(leg), scaleDenominator(decimals))
			gotRat, ok := new(big.Rat).SetString(*got)
			if !ok {
				t.Fatalf("tradeUSDVolume returned an unparseable value %q", *got)
			}
			if gotRat.Cmp(want) != 0 {
				t.Errorf("waterfall produced %s but the classifier's tier %q + decimals %d imply %s — "+
					"the two have drifted, so verify-usd-volume would check the wrong identity",
					gotRat.FloatString(10), tier, decimals, want.FloatString(10))
			}
		})
	}
}
