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

// TestAllAliasForms pins the SQL-fold projection the volume-character rollup
// binds: every NON-canonical form maps to its canonical form, and the
// canonical form itself is NOT a key (it maps to itself via the caller's
// COALESCE fallback). On the XLM-only baseline that is exactly
// {crypto:XLM → native, <XLM SAC> → native}.
func TestAllAliasForms(t *testing.T) {
	// Reset to the XLM-only baseline so the test is independent of any
	// config-derived registry a prior test installed.
	InstallAliasRegistry(nil)

	got := AllAliasForms()
	want := map[string]string{
		"crypto:XLM":     "native",
		XLMSacContractID: "native",
	}
	if len(got) != len(want) {
		t.Fatalf("AllAliasForms() = %v, want %v", got, want)
	}
	for form, canon := range want {
		if got[form] != canon {
			t.Errorf("AllAliasForms()[%q] = %q, want %q", form, got[form], canon)
		}
	}
	// The canonical form must NEVER appear as a key — a self-map would
	// fold native onto native redundantly and, worse, signal the SQL to
	// treat a canonical row as an alias.
	if _, ok := got["native"]; ok {
		t.Errorf("AllAliasForms() maps the canonical form 'native' — it must be omitted")
	}
}

// TestAliasForms_ConfigRegistry proves a configured classic↔SAC pair adds
// exactly its SAC→classic entry (SAC folds onto the classic, never the
// reverse), on top of the XLM baseline.
func TestAliasForms_ConfigRegistry(t *testing.T) {
	// USDC classic ↔ its SAC (the operator's canonical example).
	const usdcClassic = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	const usdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	reg, err := NewAliasRegistry(map[string]string{usdcSAC: usdcClassic})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	got := reg.AliasForms()
	if got[usdcSAC] != usdcClassic {
		t.Errorf("AliasForms()[USDC SAC] = %q, want %q (classic)", got[usdcSAC], usdcClassic)
	}
	if _, ok := got[usdcClassic]; ok {
		t.Errorf("AliasForms() maps the canonical classic USDC form — it must be omitted")
	}
	// XLM baseline still present.
	if got["crypto:XLM"] != "native" {
		t.Errorf("AliasForms()[crypto:XLM] = %q, want native", got["crypto:XLM"])
	}
}
