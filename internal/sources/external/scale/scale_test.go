// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scale

import (
	"fmt"
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
	if got.String() != "923787" { // 0.923787... EUR truncated at 6dp
		t.Errorf("InvertScaled(1.0825) = %s, want 923787", got)
	}
	// Identity-ish: inverting 1.0 is 1.0.
	one, _ := DecimalStringToScaledInt("1.0", 6)
	if got := InvertScaled(one, 6); got.String() != "1000000" {
		t.Errorf("InvertScaled(1.0) = %s, want 1000000", got)
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
