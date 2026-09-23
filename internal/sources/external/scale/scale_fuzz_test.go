// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scale

import (
	"encoding/hex"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// strictDecimal is the grammar DecimalStringToScaledInt must accept and
// nothing more: an optional sign, then digits with at most one point and
// at least one digit.
var strictDecimal = regexp.MustCompile(`^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)$`)

// refTruncScaled is the exact reference: s·10^d truncated toward zero.
func refTruncScaled(t *testing.T, s string, d int) *big.Int {
	t.Helper()
	body := strings.TrimLeft(s, "+-")
	if strings.HasPrefix(body, ".") {
		body = "0" + body
	}
	body = strings.TrimSuffix(body, ".")
	r, ok := new(big.Rat).SetString(body)
	if !ok {
		t.Fatalf("reference parse of %q failed", s)
	}
	num := new(big.Int).Mul(r.Num(), Pow10(d))
	q := num.Quo(num, r.Denom())
	if strings.HasPrefix(s, "-") {
		q.Neg(q)
	}
	return q
}

func clampDecimals(d uint8) int { return int(d % 19) }

func FuzzDecimalStringToScaledInt(f *testing.F) {
	for _, s := range []string{
		"1", "1.5", "0.00000001", "", "-2.5", "12345.6789", "1e5", ".5", "7",
		"0.17582", "152.34", "100.00000000", "340282366920938463463374607431768211455.123456789",
		"-", ".", "--1.5", "+1.5", "-+1", "1.", "1..5", "1.5x", "0.123456789abc", " 1", "1_000",
	} {
		f.Add(s, uint8(8))
		f.Add(s, uint8(0))
		f.Add(s, uint8(6))
	}
	f.Fuzz(func(t *testing.T, s string, dd uint8) {
		d := clampDecimals(dd)
		got, err := DecimalStringToScaledInt(s, d)
		valid := strictDecimal.MatchString(s)
		if !valid {
			if err == nil {
				t.Fatalf("DecimalStringToScaledInt(%q,%d) = %s, want error: not a decimal", s, d, got)
			}
			return
		}
		if err != nil {
			t.Fatalf("DecimalStringToScaledInt(%q,%d) unexpected err: %v", s, d, err)
		}
		if want := refTruncScaled(t, s, d); got.Cmp(want) != 0 {
			t.Fatalf("DecimalStringToScaledInt(%q,%d) = %s, want %s", s, d, got, want)
		}
	})
}

// formatScaled renders v at d decimals exactly (the inverse the parser
// must round-trip, for any width — well past int64 per ADR-0003).
func formatScaled(v *big.Int, d int) string {
	abs := new(big.Int).Abs(v).String()
	if d > 0 {
		if len(abs) <= d {
			abs = strings.Repeat("0", d-len(abs)+1) + abs
		}
		abs = abs[:len(abs)-d] + "." + abs[len(abs)-d:]
	}
	if v.Sign() < 0 {
		return "-" + abs
	}
	return abs
}

func FuzzDecimalStringRoundTrip(f *testing.F) {
	f.Add([]byte{0x01}, false, uint8(8))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, true, uint8(8))
	f.Add(big.NewInt(math.MaxInt64).Bytes(), false, uint8(6))
	f.Add(new(big.Int).Lsh(big.NewInt(1), 127).Bytes(), true, uint8(18))
	f.Fuzz(func(t *testing.T, mag []byte, neg bool, dd uint8) {
		d := clampDecimals(dd)
		v := new(big.Int).SetBytes(mag)
		if neg {
			v.Neg(v)
		}
		s := formatScaled(v, d)
		got, err := DecimalStringToScaledInt(s, d)
		if err != nil {
			t.Fatalf("round-trip %s at %d: %v", s, d, err)
		}
		if got.Cmp(v) != 0 {
			t.Fatalf("round-trip %q at %d = %s, want %s", s, d, got, v)
		}
		// One extra fractional digit must truncate toward zero, never
		// round away from it.
		if d > 0 {
			s9 := s + "9"
			if !strings.Contains(s, ".") {
				s9 = s + ".9"
			}
			got9, err := DecimalStringToScaledInt(s9, d)
			if err != nil || got9.Cmp(v) != 0 {
				t.Fatalf("over-precision %q at %d = %v, %v; want %s", s9, d, got9, err, v)
			}
		}
	})
}

func FuzzFloatToScaledInt(f *testing.F) {
	for _, v := range []float64{0, 1.25, 0.17582, 1e-9, 5e-9, 0.1, 123456.789, 1e20, -1, math.Inf(1), math.NaN(), math.SmallestNonzeroFloat64, math.MaxFloat64} {
		f.Add(v, uint8(8))
		f.Add(v, uint8(6))
	}
	f.Fuzz(func(t *testing.T, v float64, dd uint8) {
		d := clampDecimals(dd)
		got, err := FloatToScaledInt(v, d)
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			if err == nil {
				t.Fatalf("FloatToScaledInt(%v,%d) = %s, want error", v, d, got)
			}
			return
		}
		if err != nil {
			t.Fatalf("FloatToScaledInt(%v,%d): %v", v, d, err)
		}
		// Round-to-nearest: |got − v·10^d| ≤ 1/2, computed exactly.
		exact := new(big.Rat).SetFloat64(v)
		exact.Mul(exact, new(big.Rat).SetInt(Pow10(d)))
		diff := new(big.Rat).Sub(new(big.Rat).SetInt(got), exact)
		if diff.Abs(diff).Cmp(big.NewRat(1, 2)) > 0 {
			t.Fatalf("FloatToScaledInt(%v,%d) = %s, off by %s (> 1/2 ulp)", v, d, got, diff.FloatString(6))
		}
	})
}

func FuzzInvertScaled(f *testing.F) {
	f.Add([]byte{0x01}, []byte{0x02}, uint8(6))
	f.Add([]byte{0x03}, []byte{0x07}, uint8(8))
	f.Add(big.NewInt(1_085_000).Bytes(), big.NewInt(1_085_001).Bytes(), uint8(6))
	f.Add(big.NewInt(2_000_000).Bytes(), big.NewInt(3).Bytes(), uint8(3))
	f.Fuzz(func(t *testing.T, a, b []byte, dd uint8) {
		d := clampDecimals(dd)
		v1 := new(big.Int).SetBytes(a)
		v2 := new(big.Int).SetBytes(b)
		if v1.Sign() == 0 || v2.Sign() == 0 {
			return // callers skip non-positive rates
		}
		n := new(big.Int).Mul(Pow10(d), Pow10(d))
		for _, v := range []*big.Int{v1, v2} {
			r := InvertScaled(v, d)
			// Half-up nearest: −v ≤ 2(N − r·v) < v.
			e := new(big.Int).Sub(n, new(big.Int).Mul(r, v))
			e.Lsh(e, 1)
			if e.Cmp(new(big.Int).Neg(v)) < 0 || e.Cmp(v) >= 0 {
				t.Fatalf("InvertScaled(%s,%d) = %s: not the half-up nearest of 10^%d/v", v, d, r, 2*d)
			}
		}
		lo, hi := v1, v2
		if lo.Cmp(hi) > 0 {
			lo, hi = hi, lo
		}
		if InvertScaled(lo, d).Cmp(InvertScaled(hi, d)) < 0 {
			t.Fatalf("InvertScaled not non-increasing: v=%s→%s, v=%s→%s",
				lo, InvertScaled(lo, d), hi, InvertScaled(hi, d))
		}
	})
}

func FuzzSciDecimalStringToScaledInt(f *testing.F) {
	for _, s := range []string{"1.5e-7", "2.5E-3", "1e5", "0.0012", "-3e-2", "abc", "1e", "1e400", "", "1.2345678999e-1"} {
		f.Add(s, uint8(8))
		f.Add(s, uint8(6))
	}
	f.Fuzz(func(t *testing.T, s string, dd uint8) {
		d := clampDecimals(dd)
		got, err := SciDecimalStringToScaledInt(s, d)
		if !strings.ContainsAny(s, "eE") {
			// Non-exponent inputs take exactly the strict path.
			want, wantErr := DecimalStringToScaledInt(s, d)
			if (err == nil) != (wantErr == nil) || (err == nil && got.Cmp(want) != 0) {
				t.Fatalf("Sci(%q,%d) = %v,%v; strict = %v,%v", s, d, got, err, want, wantErr)
			}
			return
		}
		fv, perr := strconv.ParseFloat(s, 64)
		if perr != nil {
			if err == nil {
				t.Fatalf("Sci(%q,%d) = %s, want error (ParseFloat: %v)", s, d, got, perr)
			}
			return
		}
		if err != nil {
			t.Fatalf("Sci(%q,%d): %v", s, d, err)
		}
		// Through float64: within one ulp of the float's exact value
		// (format at d+2 rounds, then the strict parse truncates).
		exact := new(big.Rat).SetFloat64(fv)
		exact.Mul(exact, new(big.Rat).SetInt(Pow10(d)))
		diff := new(big.Rat).Sub(new(big.Rat).SetInt(got), exact)
		if diff.Abs(diff).Cmp(big.NewRat(101, 100)) >= 0 {
			t.Fatalf("Sci(%q,%d) = %s, off by %s from float value", s, d, got, diff.FloatString(6))
		}
	})
}

func FuzzSyntheticTxHash(f *testing.F) {
	for _, s := range []string{"", "ECB-EUR-USD-0001745000000", "COINGECKO-XLM-USD-00000000001745000000-overflow-tail"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, seed string) {
		h := SyntheticTxHash(seed)
		if len(h) != 64 {
			t.Fatalf("len = %d, want 64", len(h))
		}
		if h != strings.ToLower(h) {
			t.Fatalf("not lowercase: %s", h)
		}
		raw, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("not hex: %s", h)
		}
		n := min(len(seed), 32)
		if string(raw[:n]) != seed[:n] {
			t.Fatalf("hash %s does not carry seed prefix %q", h, seed[:n])
		}
		for _, b := range raw[n:] {
			if b != 0 {
				t.Fatalf("hash %s not zero-padded after %d seed bytes", h, n)
			}
		}
	})
}
