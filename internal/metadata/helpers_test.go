package metadata

import (
	"testing"
)

// ─── normaliseNumeric ─────────────────────────────────────────

func TestNormaliseNumeric(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string passes through", "100000000", "100000000"},
		{"int formatted", int(42), "42"},
		{"int64 formatted", int64(1_000_000_000), "1000000000"},
		{"nil → empty", nil, ""},
		{"unsupported type → empty", true, ""},
		{"float64 → empty (TOML lib doesn't emit floats here)", 3.14, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normaliseNumeric(tc.in); got != tc.want {
				t.Errorf("normaliseNumeric(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
