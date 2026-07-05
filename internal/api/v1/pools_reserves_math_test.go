package v1

import (
	"math/big"
	"testing"
)

// Depth math is exact integer arithmetic (ADR-0003). Expected values
// computed independently (arbitrary-precision, floor division):
//
//	x=1e9, y=2e9, fee=30bps:
//	  slip=100bps → Δx = x·70·10⁴/(9970·9900)          = 7_091_983
//	                Δy = y·9970·Δx/(x·10⁴+9970·Δx)      = 14_042_126
//	  slip=50bps  → Δx = 2_016_098, Δy = 4_012_035
func TestMaxInputWithinSlippage(t *testing.T) {
	x := big.NewInt(1_000_000_000)

	if got := maxInputWithinSlippage(x, 30, 100); got.Cmp(big.NewInt(7_091_983)) != 0 {
		t.Fatalf("maxInput(1e9, fee=30, slip=100) = %s, want 7091983", got)
	}
	if got := maxInputWithinSlippage(x, 30, 50); got.Cmp(big.NewInt(2_016_098)) != 0 {
		t.Fatalf("maxInput(1e9, fee=30, slip=50) = %s, want 2016098", got)
	}

	t.Run("tier at or below the fee yields zero", func(t *testing.T) {
		if got := maxInputWithinSlippage(x, 30, 30); got.Sign() != 0 {
			t.Fatalf("slip==fee should be 0, got %s", got)
		}
		if got := maxInputWithinSlippage(x, 30, 10); got.Sign() != 0 {
			t.Fatalf("slip<fee should be 0, got %s", got)
		}
	})

	t.Run("empty pool yields zero", func(t *testing.T) {
		if got := maxInputWithinSlippage(new(big.Int), 30, 100); got.Sign() != 0 {
			t.Fatalf("zero reserve should be 0, got %s", got)
		}
	})

	t.Run("i128-scale reserves stay exact (no float paths)", func(t *testing.T) {
		// 10^30 ≫ 2^63 — any float64 or int64 shortcut would corrupt this.
		xBig, _ := new(big.Int).SetString("1000000000000000000000000000000", 10)
		want, _ := new(big.Int).SetString("7091983019766369816520267874", 10)
		if got := maxInputWithinSlippage(xBig, 30, 100); got.Cmp(want) != 0 {
			t.Fatalf("maxInput(1e30) = %s, want %s", got, want)
		}
	})
}

func TestConstantProductOutput(t *testing.T) {
	x := big.NewInt(1_000_000_000)
	y := big.NewInt(2_000_000_000)

	if got := constantProductOutput(x, y, big.NewInt(7_091_983), 30); got.Cmp(big.NewInt(14_042_126)) != 0 {
		t.Fatalf("output = %s, want 14042126", got)
	}
	if got := constantProductOutput(x, y, big.NewInt(2_016_098), 30); got.Cmp(big.NewInt(4_012_035)) != 0 {
		t.Fatalf("output = %s, want 4012035", got)
	}
	if got := constantProductOutput(x, y, new(big.Int), 30); got.Sign() != 0 {
		t.Fatalf("zero input should output 0, got %s", got)
	}
}

// TestDepthSlippageBoundary re-derives the average-execution-price
// slippage of the computed max input with big.Rat (an independent
// formulation) and asserts the boundary property: slippage(Δx) ≤ s
// and slippage(Δx+1) > s. This guards the closed-form solve against
// sign/rounding drift without trusting the same algebra twice.
func TestDepthSlippageBoundary(t *testing.T) {
	x := big.NewInt(1_000_000_000)
	y := big.NewInt(2_000_000_000)
	const feeBps, slipBps = int64(30), int64(100)

	slippageOf := func(dx *big.Int) *big.Rat {
		// s = 1 − (Δy/Δx)/(y/x) with Δy/Δx = y·(1−f)/(x + (1−f)Δx),
		// evaluated in exact rationals (no flooring — the model curve).
		gamma := big.NewRat(10000-feeBps, 10000)
		gdx := new(big.Rat).Mul(gamma, new(big.Rat).SetInt(dx))
		execOverMid := new(big.Rat).Mul(gamma, new(big.Rat).SetInt(x))
		execOverMid.Quo(execOverMid, new(big.Rat).Add(new(big.Rat).SetInt(x), gdx))
		return new(big.Rat).Sub(big.NewRat(1, 1), execOverMid)
	}

	dx := maxInputWithinSlippage(x, feeBps, slipBps)
	_ = y // reserves-out cancels in the ratio; kept for the formula's readability
	tier := big.NewRat(slipBps, 10000)
	if slippageOf(dx).Cmp(tier) > 0 {
		t.Fatalf("slippage(maxInput) = %s exceeds the %s tier", slippageOf(dx).FloatString(8), tier.FloatString(4))
	}
	dxPlus := new(big.Int).Add(dx, big.NewInt(1))
	if slippageOf(dxPlus).Cmp(tier) <= 0 {
		t.Fatalf("maxInput is not maximal: slippage(maxInput+1) = %s is still within the tier", slippageOf(dxPlus).FloatString(8))
	}
}

func TestMidPriceString(t *testing.T) {
	cases := []struct {
		name           string
		rNum, rDen     int64
		decNum, decDen uint32
		want           string
	}{
		{"same decimals, clean ratio", 2_000_000_000, 1_000_000_000, 7, 7, "2"},
		{"decimals adjust: 6dp vs 7dp", 1_000_000, 10_000_000, 6, 7, "1"},
		{"trailing zeros trimmed", 1_000_000_000, 4_000_000_000, 7, 7, "0.25"},
		{"repeating decimal capped at 18dp", 1, 3, 0, 0, "0.333333333333333333"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := midPriceString(big.NewInt(tc.rNum), big.NewInt(tc.rDen), tc.decNum, tc.decDen)
			if got != tc.want {
				t.Fatalf("midPriceString = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBpsToPctString(t *testing.T) {
	for _, tc := range []struct {
		bps  int64
		want string
	}{{50, "0.5"}, {100, "1"}, {200, "2"}, {250, "2.5"}, {25, "0.25"}} {
		if got := bpsToPctString(tc.bps); got != tc.want {
			t.Fatalf("bpsToPctString(%d) = %q, want %q", tc.bps, got, tc.want)
		}
	}
}
