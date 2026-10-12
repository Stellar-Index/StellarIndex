package scval

import (
	"strings"
	"testing"
)

// MustEncodeSymbol / MustEncodeString are constant helpers that panic on bad input. Pin that success matches the non-panic
// variant and panics carry the "scval.MustEncode…" prefix so a crash log is traceable.

func TestMustEncodeSymbol_happyPath(t *testing.T) {
	got := MustEncodeSymbol("swap")
	want, err := EncodeSymbol("swap")
	if err != nil {
		t.Fatalf("EncodeSymbol: %v", err)
	}
	if got != want {
		t.Errorf("MustEncodeSymbol = %q, want %q", got, want)
	}
}

func TestMustEncodeSymbol_panicsOnInvalid(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on empty symbol, got none")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value not a string: %T %v", r, r)
		}
		if !strings.Contains(msg, "MustEncodeSymbol") {
			t.Errorf("panic %q missing \"MustEncodeSymbol\" prefix", msg)
		}
	}()
	_ = MustEncodeSymbol("") // empty is invalid for ScSymbol
}

func TestMustEncodeString_happyPath(t *testing.T) {
	got := MustEncodeString("SoroswapPair")
	want, err := EncodeString("SoroswapPair")
	if err != nil {
		t.Fatalf("EncodeString: %v", err)
	}
	if got != want {
		t.Errorf("MustEncodeString = %q, want %q", got, want)
	}
}
