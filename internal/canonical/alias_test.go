// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import "testing"

// TestXLMSacContractID_MatchesDerivation pins the constant to the
// DERIVATION rather than to a fixture. A wrong C-address here would put
// a foreign contract into XLM's equivalence class — i.e. would make an
// unrelated token's pool eligible to price XLM — so the literal is
// checked against Asset.SacContractID() on every run.
func TestXLMSacContractID_MatchesDerivation(t *testing.T) {
	derived, err := NativeAsset().SacContractID()
	if err != nil {
		t.Fatalf("SacContractID(native): %v", err)
	}
	if derived != XLMSacContractID {
		t.Errorf("XLMSacContractID = %q, want the derivation %q", XLMSacContractID, derived)
	}
	// orientation.go's ranking must key off the same literal.
	if nativeSAC != XLMSacContractID {
		t.Errorf("nativeSAC = %q, want XLMSacContractID %q", nativeSAC, XLMSacContractID)
	}
}

// TestXLMAliasFamily_IsValid guards the struct-literal construction in
// alias.go: the family members bypass the New* constructors, so their
// validity is asserted here instead.
func TestXLMAliasFamily_IsValid(t *testing.T) {
	for _, a := range xlmAliasFamily {
		if err := a.Validate(); err != nil {
			t.Errorf("alias family member %q invalid: %v", a.String(), err)
		}
	}
	if len(xlmAliasFamily) != 3 {
		t.Fatalf("family size = %d, want 3 (native, crypto:XLM, SAC)", len(xlmAliasFamily))
	}
	// The order IS the contract — see the AssetAliases godoc on why the
	// SAC form must be last.
	want := []string{"native", "crypto:XLM", XLMSacContractID}
	for i := range want {
		if got := xlmAliasFamily[i].String(); got != want[i] {
			t.Errorf("family[%d] = %q, want %q", i, got, want[i])
		}
	}
}

// TestAssetAliases is the contract test for the primitive that
// internal/api/v1 and internal/aggregate BOTH delegate to. It replaces
// the two hand-kept-in-lock-step copies those packages used to carry
// (C4-012/C4-013, audit-2026-07-23).
func TestAssetAliases(t *testing.T) {
	cases := map[string][]string{
		// Literal first, then the rest of the family in canonical
		// order. The SAC form is LAST for `native` and `crypto:XLM`:
		// a thin Soroban pool must never outrank SDEX or CEX depth.
		"native":         {"native", "crypto:XLM", XLMSacContractID},
		"crypto:XLM":     {"crypto:XLM", "native", XLMSacContractID},
		XLMSacContractID: {XLMSacContractID, "native", "crypto:XLM"},
	}
	for in, want := range cases {
		a, err := ParseAsset(in)
		if err != nil {
			t.Fatalf("parse %s: %v", in, err)
		}
		got := AssetAliases(a)
		if len(got) != len(want) {
			t.Fatalf("AssetAliases(%s) = %v (len %d), want len %d", in, got, len(got), len(want))
		}
		for i := range want {
			if got[i].String() != want[i] {
				t.Errorf("AssetAliases(%s)[%d] = %q, want %q", in, i, got[i].String(), want[i])
			}
		}
	}

	// A non-XLM asset returns only itself — callers loop unconditionally.
	usdc, err := NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("classic: %v", err)
	}
	if got := AssetAliases(usdc); len(got) != 1 || !got[0].Equal(usdc) {
		t.Errorf("AssetAliases(USDC) = %v, want just itself", got)
	}

	// A DIFFERENT Soroban contract is not XLM — the family must be keyed
	// on the exact SAC address, not on "is a C-address".
	other, err := NewSorobanAsset("CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC")
	if err != nil {
		t.Fatalf("soroban: %v", err)
	}
	if got := AssetAliases(other); len(got) != 1 || !got[0].Equal(other) {
		t.Errorf("AssetAliases(non-XLM contract) = %v, want just itself", got)
	}
}

// TestAssetAliasStrings pins the SQL-membership projection: same forms,
// same order. The per-source breakdown queries value a SAC leg as XLM,
// so a form set missing the SAC undercounts Soroban XLM volume (C4-012).
func TestAssetAliasStrings(t *testing.T) {
	got := AssetAliasStrings(NativeAsset())
	want := []string{"native", "crypto:XLM", XLMSacContractID}
	if len(got) != len(want) {
		t.Fatalf("AssetAliasStrings(native) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AssetAliasStrings(native)[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
