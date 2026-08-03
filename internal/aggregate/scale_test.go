package aggregate_test

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// scaleTrade builds a Trade at an explicit source with big-int amounts, so a
// test can express the smallest-unit magnitudes a per-source scale produces
// (10^10 stroops = 1000 XLM at 7dp, etc.) without int64 overflow worries.
func scaleTrade(source string, base, quote *big.Int) canonical.Trade {
	return canonical.Trade{
		Source:      source,
		Ledger:      1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		OpIndex:     0,
		Timestamp:   time.Unix(0, 0).UTC(),
		BaseAmount:  canonical.NewAmount(base),
		QuoteAmount: canonical.NewAmount(quote),
	}
}

// scaleDecimals is the test resolver: on-chain 7, CEX 8, FX 6 — the same
// split external.Lookup(source).AmountScaleDecimals() returns in production.
func scaleDecimals(source string) int {
	switch source {
	case "onchain":
		return 7
	case "cex":
		return 8
	case "fx":
		return 6
	default:
		return 8
	}
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// TestNormalizeAmountScale_MixedWindowFixesVWAP is the headline case: 1000 XLM
// @0.10 on-chain (7dp) + 1000 XLM @0.12 CEX (8dp), equal real volume, so the
// true volume-weighted price is the midpoint 0.11. Over the RAW cross-scale
// amounts the CEX leg's base weight (10^11) is 10× the on-chain leg's (10^10),
// so the un-normalized VWAP is dragged to 0.1181…; normalizing lifts the
// on-chain leg to the common 8dp scale and restores 0.11 exactly.
func TestNormalizeAmountScale_MixedWindowFixesVWAP(t *testing.T) {
	// 1000 XLM at 7dp = 1000·10^7 = 10^10; 100 USDC quote = 10^9  → 0.10.
	onchain := scaleTrade("onchain",
		new(big.Int).Mul(big.NewInt(1000), pow10(7)),
		new(big.Int).Mul(big.NewInt(100), pow10(7)))
	// 1000 XLM at 8dp = 1000·10^8 = 10^11; 120 USDT quote = 120·10^8 → 0.12.
	cex := scaleTrade("cex",
		new(big.Int).Mul(big.NewInt(1000), pow10(8)),
		new(big.Int).Mul(big.NewInt(120), pow10(8)))

	raw := []canonical.Trade{onchain, cex}

	// The bug, pinned: over raw amounts VWAP = (10^9+1.2·10^10)/(10^10+10^11)
	// = 13/110, the 10×-over-weighted-CEX answer.
	rawVWAP, err := aggregate.VWAP(raw)
	if err != nil {
		t.Fatalf("VWAP(raw): %v", err)
	}
	if rawVWAP.Cmp(big.NewRat(13, 110)) != 0 {
		t.Fatalf("precondition: raw VWAP = %s, want 13/110 (the un-normalized bug value)",
			rawVWAP.FloatString(10))
	}

	// The fix: normalize, then VWAP = 0.11 exactly.
	norm := aggregate.NormalizeAmountScale(raw, scaleDecimals)
	got, err := aggregate.VWAP(norm)
	if err != nil {
		t.Fatalf("VWAP(normalized): %v", err)
	}
	if got.Cmp(big.NewRat(11, 100)) != 0 {
		t.Errorf("normalized VWAP = %s, want 0.11 (equal-real-volume midpoint); "+
			"%s is the raw 10×-over-weighted-CEX value",
			got.FloatString(10), rawVWAP.FloatString(10))
	}
}

// TestNormalizeAmountScale_UniformWindowByteIdentical proves the critical
// invariant: a single-scale window (all-CEX or all-on-chain) is returned
// byte-for-byte unchanged — every amount, not just the resulting price — so
// the fix cannot move a served value on the common uniform path.
func TestNormalizeAmountScale_UniformWindowByteIdentical(t *testing.T) {
	for _, src := range []string{"cex", "onchain", "fx"} {
		src := src
		t.Run(src, func(t *testing.T) {
			in := []canonical.Trade{
				scaleTrade(src, big.NewInt(20), big.NewInt(40)),
				scaleTrade(src, big.NewInt(100), big.NewInt(300)),
				scaleTrade(src, big.NewInt(7), big.NewInt(3)),
			}
			out := aggregate.NormalizeAmountScale(in, scaleDecimals)
			if len(out) != len(in) {
				t.Fatalf("len(out) = %d, want %d", len(out), len(in))
			}
			for i := range in {
				if out[i].BaseAmount.Cmp(in[i].BaseAmount) != 0 {
					t.Errorf("trade %d base changed: %s -> %s (uniform window must be byte-identical)",
						i, in[i].BaseAmount, out[i].BaseAmount)
				}
				if out[i].QuoteAmount.Cmp(in[i].QuoteAmount) != 0 {
					t.Errorf("trade %d quote changed: %s -> %s (uniform window must be byte-identical)",
						i, in[i].QuoteAmount, out[i].QuoteAmount)
				}
			}
		})
	}
}

// TestNormalizeAmountScale_PreservesPerTradeRatio confirms the price ratio of
// every individual trade is untouched — only its WEIGHT is rescaled. Base and
// quote are lifted by the same factor, so quote/base is invariant.
func TestNormalizeAmountScale_PreservesPerTradeRatio(t *testing.T) {
	in := []canonical.Trade{
		scaleTrade("onchain", new(big.Int).Mul(big.NewInt(3), pow10(7)), new(big.Int).Mul(big.NewInt(7), pow10(7))),
		scaleTrade("cex", new(big.Int).Mul(big.NewInt(5), pow10(8)), new(big.Int).Mul(big.NewInt(11), pow10(8))),
		scaleTrade("fx", new(big.Int).Mul(big.NewInt(2), pow10(6)), new(big.Int).Mul(big.NewInt(9), pow10(6))),
	}
	out := aggregate.NormalizeAmountScale(in, scaleDecimals)
	for i := range in {
		want := new(big.Rat).SetFrac(in[i].QuoteAmount.BigInt(), in[i].BaseAmount.BigInt())
		got := new(big.Rat).SetFrac(out[i].QuoteAmount.BigInt(), out[i].BaseAmount.BigInt())
		if got.Cmp(want) != 0 {
			t.Errorf("trade %d ratio changed: %s -> %s", i, want.FloatString(12), got.FloatString(12))
		}
	}
}

// TestNormalizeAmountScale_DoesNotMutateInput guards against corrupting a
// shared read cache: the input slice's amounts must be left exactly as they
// were (the helper reads via Amount.BigInt, which copies, and writes via
// canonical.NewAmount, which copies).
func TestNormalizeAmountScale_DoesNotMutateInput(t *testing.T) {
	origBase := new(big.Int).Mul(big.NewInt(1000), pow10(7))
	origQuote := new(big.Int).Mul(big.NewInt(100), pow10(7))
	in := []canonical.Trade{
		scaleTrade("onchain", new(big.Int).Set(origBase), new(big.Int).Set(origQuote)),
		scaleTrade("cex", new(big.Int).Mul(big.NewInt(1000), pow10(8)), new(big.Int).Mul(big.NewInt(120), pow10(8))),
	}
	_ = aggregate.NormalizeAmountScale(in, scaleDecimals)
	if in[0].BaseAmount.BigInt().Cmp(origBase) != 0 {
		t.Errorf("input base mutated: got %s, want %s", in[0].BaseAmount, origBase)
	}
	if in[0].QuoteAmount.BigInt().Cmp(origQuote) != 0 {
		t.Errorf("input quote mutated: got %s, want %s", in[0].QuoteAmount, origQuote)
	}
}

// TestNormalizeAmountScale_TWAPUnaffected substantiates the scoping call that
// TWAP does not carry this bug: TWAP weights each trade's (scale-invariant)
// price by TIME, never by amount, so normalizing the amounts leaves the TWAP
// output identical on the same mixed window that moves the VWAP.
func TestNormalizeAmountScale_TWAPUnaffected(t *testing.T) {
	t0 := time.Unix(0, 0).UTC()
	a := scaleTrade("onchain", new(big.Int).Mul(big.NewInt(1000), pow10(7)), new(big.Int).Mul(big.NewInt(100), pow10(7)))
	a.Timestamp = t0
	b := scaleTrade("cex", new(big.Int).Mul(big.NewInt(1000), pow10(8)), new(big.Int).Mul(big.NewInt(120), pow10(8)))
	b.Timestamp = t0.Add(time.Minute)
	windowEnd := t0.Add(2 * time.Minute)

	raw := []canonical.Trade{a, b}
	norm := aggregate.NormalizeAmountScale(raw, scaleDecimals)

	rawTWAP, err := aggregate.TWAP(raw, windowEnd)
	if err != nil {
		t.Fatalf("TWAP(raw): %v", err)
	}
	normTWAP, err := aggregate.TWAP(norm, windowEnd)
	if err != nil {
		t.Fatalf("TWAP(normalized): %v", err)
	}
	if rawTWAP.Cmp(normTWAP) != 0 {
		t.Errorf("TWAP moved under normalization (%s -> %s); TWAP is time-weighted and "+
			"must be scale-invariant", rawTWAP.FloatString(12), normTWAP.FloatString(12))
	}
}
