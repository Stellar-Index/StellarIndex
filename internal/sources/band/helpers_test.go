package band

import (
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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

// Oracle capture-totality (PR-2): an unknown symbol is no longer an
// error — it resolves to a verbatim raw:<symbol> asset so the slot is
// recorded. Only an unrepresentable symbol (which an ScSymbol can
// never be) is refused, as canonical.ErrInvalidAsset.
func TestSymbolToAsset_unknownIsRaw(t *testing.T) {
	a, err := symbolToAsset("DOGEMOON")
	if err != nil {
		t.Fatalf("symbolToAsset(DOGEMOON): %v", err)
	}
	if a.Type != canonical.AssetOracleRaw || a.Code != "DOGEMOON" || a.IsMapped() {
		t.Errorf("symbolToAsset(DOGEMOON) = %+v, want raw:DOGEMOON with IsMapped()=false", a)
	}
	if _, err := symbolToAsset("has space"); !errors.Is(err, canonical.ErrInvalidAsset) {
		t.Errorf("symbolToAsset(unrepresentable) err = %v, want canonical.ErrInvalidAsset", err)
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
