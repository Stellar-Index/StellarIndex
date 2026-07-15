// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scval_test

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

func symVal(t *testing.T, s string) xdr.ScVal {
	t.Helper()
	sym := xdr.ScSymbol(s)
	v, err := xdr.NewScVal(xdr.ScValTypeScvSymbol, sym)
	if err != nil {
		t.Fatalf("NewScVal symbol: %v", err)
	}
	return v
}

func u32Val(t *testing.T, n uint32) xdr.ScVal {
	t.Helper()
	v, err := xdr.NewScVal(xdr.ScValTypeScvU32, xdr.Uint32(n))
	if err != nil {
		t.Fatalf("NewScVal u32: %v", err)
	}
	return v
}

func i128Val(t *testing.T, hi int64, lo uint64) xdr.ScVal {
	t.Helper()
	v, err := xdr.NewScVal(xdr.ScValTypeScvI128, xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)})
	if err != nil {
		t.Fatalf("NewScVal i128: %v", err)
	}
	return v
}

// TestDisplay_Vec is the regression test for the refactor that orphaned the
// vec/map container cases out of display() (commits c5e7006e + 01e938a1) —
// containers were silently degrading to bare type names ("Vec" / "Map").
func TestDisplay_Vec(t *testing.T) {
	vec := xdr.ScVec{symVal(t, "swap"), u32Val(t, 7)}
	v, err := xdr.NewScVal(xdr.ScValTypeScvVec, &vec)
	if err != nil {
		t.Fatalf("NewScVal vec: %v", err)
	}
	if got := scval.Display(v); got != "[swap, 7]" {
		t.Errorf("Display(vec) = %q, want %q", got, "[swap, 7]")
	}
}

func TestDisplay_Map(t *testing.T) {
	m := xdr.ScMap{
		{Key: symVal(t, "amount"), Val: i128Val(t, 0, 1234500)},
	}
	v, err := xdr.NewScVal(xdr.ScValTypeScvMap, &m)
	if err != nil {
		t.Fatalf("NewScVal map: %v", err)
	}
	if got := scval.Display(v); got != "{amount: 1234500}" {
		t.Errorf("Display(map) = %q, want %q", got, "{amount: 1234500}")
	}
}

func TestDisplay_NestedDepthCap(t *testing.T) {
	// Build a vec nested 6 deep; depth cap is 3 so the innermost
	// levels must degrade to "…" instead of recursing forever.
	inner := symVal(t, "x")
	v := inner
	for i := 0; i < 6; i++ {
		vec := xdr.ScVec{v}
		nv, err := xdr.NewScVal(xdr.ScValTypeScvVec, &vec)
		if err != nil {
			t.Fatalf("NewScVal vec: %v", err)
		}
		v = nv
	}
	got := scval.Display(v)
	if got != "[[[[…]]]]" {
		t.Errorf("Display(deep vec) = %q, want depth-capped %q", got, "[[[[…]]]]")
	}
}

// TestDisplay_I128 pins the ADR-0003 invariant at the display layer: a value
// above 2^63 renders as the full decimal string, never a truncated int64.
func TestDisplay_I128(t *testing.T) {
	// hi=1, lo=0 → 2^64 = 18446744073709551616.
	if got := scval.Display(i128Val(t, 1, 0)); got != "18446744073709551616" {
		t.Errorf("Display(i128 2^64) = %q", got)
	}
}
