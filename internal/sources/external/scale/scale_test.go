// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scale

import "testing"

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
