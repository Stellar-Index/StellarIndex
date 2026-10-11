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

// TestNormalizeAmountScale_MixedWindowFixesVWAP is the headline case: 1000 XLM at 0.10 (7dp) plus 1000 XLM at 0.12 (8dp) must price at 0.11, not the raw-weighted 0.1181.
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
	rawVWAP, err := aggregate.VWAPOf(raw)
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

// TestNormalizeAmountScale_UniformWindowByteIdentical pins that a single-scale window is returned byte-for-byte unchanged, every amount and not just the price.
func TestNormalizeAmountScale_UniformWindowByteIdentical(t *testing.T) {
	for _, src := range []string{"cex", "onchain", "fx"} {
		src := src
		t.Run(src, func(t *testing.T) {
			in := []canonical.Trade{
				scaleTrade(src, big.NewInt(20), big.NewInt(40)),
				scaleTrade(src, big.NewInt(100), big.NewInt(300)),
				scaleTrade(src, big.NewInt(7), big.NewInt(3)),
			}
			out := aggregate.NormalizeAmountScale(in, scaleDecimals).Trades()
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

// TestNormalizeAmountScale_PreservesPerTradeRatio confirms every trade's quote/base ratio is untouched; only its weight is rescaled.
func TestNormalizeAmountScale_PreservesPerTradeRatio(t *testing.T) {
	in := []canonical.Trade{
		scaleTrade("onchain", new(big.Int).Mul(big.NewInt(3), pow10(7)), new(big.Int).Mul(big.NewInt(7), pow10(7))),
		scaleTrade("cex", new(big.Int).Mul(big.NewInt(5), pow10(8)), new(big.Int).Mul(big.NewInt(11), pow10(8))),
		scaleTrade("fx", new(big.Int).Mul(big.NewInt(2), pow10(6)), new(big.Int).Mul(big.NewInt(9), pow10(6))),
	}
	out := aggregate.NormalizeAmountScale(in, scaleDecimals).Trades()
	for i := range in {
		want := new(big.Rat).SetFrac(in[i].QuoteAmount.BigInt(), in[i].BaseAmount.BigInt())
		got := new(big.Rat).SetFrac(out[i].QuoteAmount.BigInt(), out[i].BaseAmount.BigInt())
		if got.Cmp(want) != 0 {
			t.Errorf("trade %d ratio changed: %s -> %s", i, want.FloatString(12), got.FloatString(12))
		}
	}
}

// TestNormalizeAmountScale_DoesNotMutateInput guards a shared read cache: the input amounts must be left exactly as they were.
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

// TestNormalizeAmountScale_TWAPUnaffected pins that TWAP weights by time, not amount, so normalizing leaves it identical on a mixed window.
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
	normTWAP, err := aggregate.TWAP(norm.Trades(), windowEnd)
	if err != nil {
		t.Fatalf("TWAP(normalized): %v", err)
	}
	if rawTWAP.Cmp(normTWAP) != 0 {
		t.Errorf("TWAP moved under normalization (%s -> %s); TWAP is time-weighted and "+
			"must be scale-invariant", rawTWAP.FloatString(12), normTWAP.FloatString(12))
	}
}

// TestScaledWindow_FilterKeepsLiftTarget pins that the outlier filter cannot strip a window's scale: survivors lifted x10 still state 8dp.
func TestScaledWindow_FilterKeepsLiftTarget(t *testing.T) {
	var raw []canonical.Trade
	for _, q := range []int64{100, 101, 99, 100} {
		raw = append(raw, scaleTrade("onchain",
			new(big.Int).Mul(big.NewInt(1000), pow10(7)), new(big.Int).Mul(big.NewInt(q), pow10(7))))
	}
	raw = append(raw, scaleTrade("cex",
		new(big.Int).Mul(big.NewInt(1000), pow10(8)), new(big.Int).Mul(big.NewInt(5000), pow10(8))))

	window := aggregate.NormalizeAmountScale(raw, scaleDecimals)
	if window.Decimals() != 8 {
		t.Fatalf("Decimals() = %d, want 8 (the cex scale)", window.Decimals())
	}
	kept := window.FilterOutliers(3)
	if kept.Len() != 4 {
		t.Fatalf("kept %d trades, want 4 (the cex print is the outlier)", kept.Len())
	}
	if kept.Decimals() != 8 {
		t.Errorf("Decimals() after filter = %d, want 8: the survivors are still lifted to it", kept.Decimals())
	}
	if got, want := kept.Trades()[0].BaseAmount.BigInt(), new(big.Int).Mul(big.NewInt(1000), pow10(8)); got.Cmp(want) != 0 {
		t.Errorf("survivor base = %s, want %s (lifted once, not un-lifted)", got, want)
	}
	got, err := aggregate.VWAP(kept)
	if err != nil {
		t.Fatalf("VWAP: %v", err)
	}
	if got.Cmp(big.NewRat(1, 10)) != 0 {
		t.Errorf("VWAP = %s, want 0.1", got.FloatString(10))
	}
}

// TestNormalizeAmountScale_UniformWindowStatesItsScale pins Decimals() on windows nothing was lifted in, including the empty one.
func TestNormalizeAmountScale_UniformWindowStatesItsScale(t *testing.T) {
	for src, want := range map[string]int{"cex": 8, "onchain": 7, "fx": 6} {
		w := aggregate.NormalizeAmountScale([]canonical.Trade{scaleTrade(src, big.NewInt(2), big.NewInt(3))}, scaleDecimals)
		if w.Decimals() != want {
			t.Errorf("%s: Decimals() = %d, want %d", src, w.Decimals(), want)
		}
	}
	if w := aggregate.NormalizeAmountScale(nil, scaleDecimals); w.Len() != 0 || w.Decimals() != 0 {
		t.Errorf("empty window = (len %d, decimals %d), want (0, 0)", w.Len(), w.Decimals())
	}
}
