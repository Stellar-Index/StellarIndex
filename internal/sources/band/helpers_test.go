package band

import (
	"errors"
	"testing"
)

// ─── symbolToAsset ────────────────────────────────────────────

func TestSymbolToAsset_crypto(t *testing.T) {
	a, err := symbolToAsset("BTC")
	if err != nil {
		t.Fatalf("symbolToAsset(BTC): %v", err)
	}
	if a.Code != "BTC" {
		t.Errorf("Code = %q, want \"BTC\"", a.Code)
	}
}

func TestSymbolToAsset_fiat(t *testing.T) {
	a, err := symbolToAsset("USD")
	if err != nil {
		t.Fatalf("symbolToAsset(USD): %v", err)
	}
	if a.Code != "USD" {
		t.Errorf("Code = %q, want \"USD\"", a.Code)
	}
}

func TestSymbolToAsset_unknown(t *testing.T) {
	_, err := symbolToAsset("DOGEMOON")
	if !errors.Is(err, ErrUnknownSymbol) {
		t.Errorf("error = %v, want ErrUnknownSymbol chain", err)
	}
}

// ─── pickObserver ─────────────────────────────────────────────

func TestPickObserver(t *testing.T) {
	cases := []struct {
		name  string
		opSrc string
		txSrc string
		want  string
	}{
		{"opSource wins", "GOPSRC", "GTXSRC", "GOPSRC"},
		{"empty opSource falls back to tx", "", "GTXSRC", "GTXSRC"},
		{"both empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickObserver(tc.opSrc, tc.txSrc); got != tc.want {
				t.Errorf("pickObserver(%q, %q) = %q, want %q",
					tc.opSrc, tc.txSrc, got, tc.want)
			}
		})
	}
}
