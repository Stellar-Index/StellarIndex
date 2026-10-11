package exchangeratesapi

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// scale.SciDecimalStringToScaledInt is the precision-preserving converter for
// ExchangeRatesAPI quotes. Unlike the strict form, it ACCEPTS scientific notation (small
// inverted rates such as "2e-10"). Both branches are pinned from this venue's side so the
// poller cannot switch to the strict form and lose exotic-pair rates, or stop rejecting
// empty/garbage input.

func TestDecimalStringToScaledInt_edges(t *testing.T) {
	cases := []struct {
		in        string
		decimals  int
		want      string
		wantError bool
	}{
		{"1.0", 8, "100000000", false},
		{"-0.5", 8, "-50000000", false},
		{".75", 8, "75000000", false},
		{"1.123456789", 8, "112345678", false},
		{"42", 0, "42", false},
		{"2e10", 8, "2000000000000000000", false}, // sci-notation accepted
		{"", 8, "", true},
		{"NaN", 8, "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := scale.SciDecimalStringToScaledInt(c.in, c.decimals)
			if c.wantError {
				if err == nil {
					t.Errorf("expected error for %q, got %v", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != c.want {
				t.Errorf("got %s, want %s", got.String(), c.want)
			}
		})
	}
}
