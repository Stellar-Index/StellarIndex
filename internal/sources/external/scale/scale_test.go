// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scale

import (
	"fmt"
	"math/big"
	"strings"
	"testing"
)

func TestDecimalStringToScaledInt(t *testing.T) {
	cases := []struct {
		s        string
		decimals int
		want     string
		wantErr  bool
	}{
		{"1", 8, "100000000", false},
		{"1.5", 8, "150000000", false},
		{"0.00000001", 8, "1", false},
		{"", 8, "", true},
		{"-2.5", 6, "-2500000", false},
		{"12345.6789", 2, "1234567", false}, // over-precision truncates
		{"1e5", 8, "", true},                // scientific rejected
		{".5", 8, "50000000", false},
		{"7", 0, "7", false},
	}
	for _, c := range cases {
		got, err := DecimalStringToScaledInt(c.s, c.decimals)
		if c.wantErr {
			if err == nil {
				t.Errorf("DecimalStringToScaledInt(%q,%d) = %v, want error", c.s, c.decimals, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("DecimalStringToScaledInt(%q,%d) unexpected err: %v", c.s, c.decimals, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("DecimalStringToScaledInt(%q,%d) = %s, want %s", c.s, c.decimals, got.String(), c.want)
		}
	}
}

func TestFloatToScaledInt(t *testing.T) {
	got, err := FloatToScaledInt(1.25, 8)
	if err != nil || got.String() != "125000000" {
		t.Fatalf("FloatToScaledInt(1.25,8) = %v, %v; want 125000000", got, err)
	}
	if _, err := FloatToScaledInt(-1, 8); err == nil {
		t.Error("FloatToScaledInt(-1,8) = nil err, want error on negative")
	}
}

func TestPow10(t *testing.T) {
	if Pow10(0).String() != "1" || Pow10(6).String() != "1000000" {
		t.Fatalf("Pow10 wrong: 0=%s 6=%s", Pow10(0), Pow10(6))
	}
}

// SyntheticTxHash must preserve the historical truncated-hex form
// byte-for-byte: tx_hash is the dedup identity of persisted
// oracle_updates rows, so a derivation change would re-insert
// history under new identities. The reference loop below is the
// per-poller implementation this helper replaced.
func TestSyntheticTxHash_matchesLegacyForm(t *testing.T) {
	legacy := func(s string) string {
		var hex strings.Builder
		hex.Grow(64)
		for _, b := range []byte(s) {
			fmt.Fprintf(&hex, "%02x", b)
			if hex.Len() >= 64 {
				break
			}
		}
		for hex.Len() < 64 {
			hex.WriteByte('0')
		}
		return hex.String()[:64]
	}

	seeds := []string{
		"ECB-USD-EUR-00000000001745539200",    // exactly 32 bytes -> 64 hex
		"PGFX-USD-EUR-00000000001745539200",   // >32 bytes -> truncated
		"XRATES-USD-EUR-00000000001745539200", // >32 bytes -> truncated
		"short",                               // <32 bytes -> zero-padded
		"",
	}
	for _, seed := range seeds {
		got := SyntheticTxHash(seed)
		want := legacy(seed)
		if got != want {
			t.Errorf("SyntheticTxHash(%q) = %s, want legacy form %s", seed, got, want)
		}
		if len(got) != 64 {
			t.Errorf("SyntheticTxHash(%q) len = %d, want 64", seed, len(got))
		}
	}
}

func TestInvertScaled(t *testing.T) {
	// 1 EUR = 1.0825 USD at 6dp -> price of USD in EUR = 1/1.0825.
	v, err := FloatToScaledInt(1.0825, 6)
	if err != nil {
		t.Fatalf("FloatToScaledInt: %v", err)
	}
	got := InvertScaled(v, 6)
	// 1/1.0825 = 0.9237875288… — the 6dp digit is followed by .5288, so
	// half-up gives 923788. This assertion previously read 923787 and
	// described it as "truncated at 6dp": it was pinning the truncation
	// bias fixed in MNY-06, not an intended value. Updated deliberately —
	// unlike a test that encodes documented behaviour, this one encoded
	// the defect.
	if got.String() != "923788" {
		t.Errorf("InvertScaled(1.0825) = %s, want 923788 (0.9237875288… rounded half-up)", got)
	}
	// Identity-ish: inverting 1.0 is 1.0.
	one, _ := DecimalStringToScaledInt("1.0", 6)
	if got := InvertScaled(one, 6); got.String() != "1000000" {
		t.Errorf("InvertScaled(1.0) = %s, want 1000000", got)
	}
}

// TestInvertScaledRoundsHalfUp pins the MNY-06 fix: the inverse is rounded
// half-up, not truncated toward zero.
//
// Why this matters enough to test at all, given the error is ≤1 ulp: a
// truncation bias is one-signed. Every inverted rate, on every poll, on all
// three FX venues that call this, lands at or below the true value and never
// above it — so it does not average out the way symmetric rounding error
// does. The cases below are chosen so truncation and half-up give provably
// different answers, in exact integer arithmetic with no float anywhere.
//
// Proven red: with the old `Div(Mul(p,p), v)` the >0.5 and ==0.5 cases fail
// (12 vs 13, 923787 vs 923788).
func TestInvertScaledRoundsHalfUp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		v        int64
		decimals int
		want     string
		why      string
	}{
		// 10^2 / 8 = 12.5 exactly — the half-way case. Half-up rounds away
		// from zero, so 13; truncation gives 12.
		{"exactly one half rounds up", 8, 1, "13", "100/8 = 12.5"},
		// 10^2 / 3 = 33.33… — below the half, so both agree.
		{"below one half rounds down", 3, 1, "33", "100/3 = 33.33…"},
		// 10^2 / 7 = 14.28… — below the half.
		{"well below one half rounds down", 7, 1, "14", "100/7 = 14.28…"},
		// 10^2 / 6 = 16.66… — above the half, so 17; truncation gives 16.
		{"above one half rounds up", 6, 1, "17", "100/6 = 16.66…"},
		// Exact division must be untouched by the rounding term.
		{"exact division is unchanged", 10, 1, "10", "100/10 = 10"},
		{"exact division at 6dp", 1_000_000, 6, "1000000", "10^12/10^6 = 10^6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := InvertScaled(big.NewInt(tc.v), tc.decimals)
			if got.String() != tc.want {
				t.Errorf("InvertScaled(%d, %d) = %s, want %s (%s)",
					tc.v, tc.decimals, got, tc.want, tc.why)
			}
		})
	}
}

// TestInvertScaledMatchesRedstoneReciprocal guards against the two
// implementations of "reciprocal at the same fixed-point scale" drifting
// apart again. redstone.reciprocalAtScale already rounded half-up; this one
// truncated, and nothing compared them — which is how MNY-06 survived. The
// formula is re-derived here rather than imported so this stays a real
// cross-check and not a tautology (importing redstone from scale would also
// invert the dependency direction).
func TestInvertScaledMatchesRedstoneReciprocal(t *testing.T) {
	redstoneFormula := func(v *big.Int, decimals int) *big.Int {
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
		num := new(big.Int).Mul(scale, scale)
		twoNum := new(big.Int).Lsh(num, 1)
		twoNum.Add(twoNum, v)
		twoR := new(big.Int).Lsh(v, 1)
		return twoNum.Quo(twoNum, twoR)
	}
	// Spread across scales and magnitudes, including the awkward ones:
	// v=1 (inverse is the full scale), v just under and just over the scale.
	for _, decimals := range []int{2, 6, 8} {
		for _, v := range []int64{1, 3, 7, 8, 9, 99, 12345, 99_999_999, 100_000_001} {
			bv := big.NewInt(v)
			want := redstoneFormula(bv, decimals)
			if got := InvertScaled(bv, decimals); got.Cmp(want) != 0 {
				t.Errorf("InvertScaled(%d, %d) = %s, redstone reciprocal = %s — "+
					"the two fixed-point reciprocal implementations have diverged again",
					v, decimals, got, want)
			}
		}
	}
}

func TestSciDecimalStringToScaledInt(t *testing.T) {
	// Strict form rejects scientific notation; Sci form accepts it.
	if _, err := DecimalStringToScaledInt("2e10", 8); err == nil {
		t.Error("strict form should reject scientific notation")
	}
	got, err := SciDecimalStringToScaledInt("2e10", 8)
	if err != nil {
		t.Fatalf("SciDecimalStringToScaledInt(2e10): %v", err)
	}
	if got.String() != "2000000000000000000" {
		t.Errorf("got %s, want 2000000000000000000", got)
	}
	// Non-sci inputs go through the strict path unchanged.
	got, err = SciDecimalStringToScaledInt("1.5", 8)
	if err != nil || got.String() != "150000000" {
		t.Errorf("got %v/%v, want 150000000", got, err)
	}
	if _, err := SciDecimalStringToScaledInt("", 8); err == nil {
		t.Error("empty string must error")
	}
	if _, err := SciDecimalStringToScaledInt("eeee", 8); err == nil {
		t.Error("garbage sci input must error")
	}
}
