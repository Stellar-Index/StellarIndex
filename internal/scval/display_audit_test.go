package scval

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// Cold audit 2026-08-04. Three Display defects, each reachable from
// contract-controlled or default-constructed input.
func TestDisplay_auditRegressions(t *testing.T) {
	t.Run("zero ScVal does not panic", func(t *testing.T) {
		// ScValTypeScvBool == 0, so a default-constructed ScVal reports
		// Type==Bool with a nil B arm. MapField returns exactly this on a
		// miss, so logging the value on a miss path panicked the goroutine.
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Display(xdr.ScVal{}) panicked: %v", r)
			}
		}()
		if got := Display(xdr.ScVal{}); got == "" {
			t.Error("Display(zero) returned empty string")
		}
	})

	t.Run("U256 renders its value, not its type name", func(t *testing.T) {
		v := xdr.ScVal{
			Type: xdr.ScValTypeScvU256,
			U256: &xdr.UInt256Parts{HiHi: 0, HiLo: 0, LoHi: 1, LoLo: 42},
		}
		// (1 << 64) + 42
		const want = "18446744073709551658"
		if got := Display(v); got != want {
			t.Errorf("Display(U256) = %q, want %q — the repo decodes u256 for Redstone prices, so rendering the bare word \"U256\" drops the number entirely", got, want)
		}
	})

	t.Run("I256 renders a negative value", func(t *testing.T) {
		v := xdr.ScVal{
			Type: xdr.ScValTypeScvI256,
			I256: &xdr.Int256Parts{HiHi: -1, HiLo: 0xFFFFFFFFFFFFFFFF, LoHi: 0xFFFFFFFFFFFFFFFF, LoLo: 0xFFFFFFFFFFFFFFFF},
		}
		if got := Display(v); got != "-1" {
			t.Errorf("Display(I256 all-ones) = %q, want -1", got)
		}
	})

	t.Run("truncation cuts on a rune boundary", func(t *testing.T) {
		// "a" + 100 x "é" puts a 2-byte rune astride byte 120.
		s := "a" + strings.Repeat("é", 100)
		v := xdr.ScVal{Type: xdr.ScValTypeScvString, Str: (*xdr.ScString)(&s)}
		got := Display(v)
		if !utf8.ValidString(got) {
			t.Errorf("Display produced invalid UTF-8: %q — the string is contract-supplied, so the input is attacker-chosen", got)
		}
	})
}
