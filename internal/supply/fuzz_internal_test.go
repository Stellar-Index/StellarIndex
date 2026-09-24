package supply

import (
	"math"
	"math/big"
	"testing"
)

func FuzzStroopsToXLM(f *testing.F) {
	f.Add([]byte{}, false)
	f.Add(big.NewInt(500_018_068_120_000_000).Bytes(), false)
	f.Add(big.NewInt(1).Bytes(), true)
	f.Add(new(big.Int).Lsh(big.NewInt(1), 130).Bytes(), false)
	f.Fuzz(func(t *testing.T, b []byte, neg bool) {
		x := new(big.Int).SetBytes(b)
		if neg {
			x.Neg(x)
		}
		got := stroopsToXLM(x)
		if math.IsNaN(got) || (got < 0) != (x.Sign() < 0) || (got == 0 && x.Sign() != 0 && x.BitLen() < 900) {
			t.Fatalf("stroopsToXLM(%s) = %v: wrong sign or lost magnitude", x, got)
		}
		// Whole lumens below 2^53 convert exactly: no int64 or float
		// shortcut on the way.
		q, r := new(big.Int).QuoRem(x, big.NewInt(10_000_000), new(big.Int))
		if r.Sign() == 0 && q.IsInt64() && math.Abs(float64(q.Int64())) < 1<<53 && got != float64(q.Int64()) {
			t.Fatalf("stroopsToXLM(%s) = %v, want exactly %d", x, got, q.Int64())
		}
		// Monotone: one more stroop never reads as fewer lumens.
		if next := stroopsToXLM(new(big.Int).Add(x, big.NewInt(1))); next < got {
			t.Fatalf("stroopsToXLM not monotone at %s: %v then %v", x, got, next)
		}
		// Correctly rounded. Bounded to 400 bits so the 512-bit
		// intermediate cannot double-round across a float64 half-way point.
		if x.BitLen() > 400 {
			return
		}
		exact := new(big.Rat).SetFrac(x, big.NewInt(10_000_000))
		ref, _ := new(big.Float).SetPrec(512).SetRat(exact).Float64()
		if got != ref {
			t.Fatalf("stroopsToXLM(%s) = %v, want %v", x, got, ref)
		}
	})
}

func FuzzLedgerGap(f *testing.F) {
	f.Add(uint32(0), uint32(0))
	f.Add(uint32(0), uint32(math.MaxUint32))
	f.Add(uint32(100), uint32(99))
	f.Fuzz(func(t *testing.T, a, b uint32) {
		want := int64(a) - int64(b)
		if want < 0 {
			want = -want
		}
		if got := ledgerGap(a, b); int64(got) != want || ledgerGap(b, a) != got {
			t.Fatalf("ledgerGap(%d, %d) = %d, want %d", a, b, got, want)
		}
	})
}
