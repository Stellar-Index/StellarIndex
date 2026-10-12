// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"testing"
)

// Pins the XLM SAC constant to the derivation, not a fixture, by checking it against Asset.SacContractID() every run. A
// wrong C-address would put a foreign contract in XLM's equivalence class and let an unrelated token's pool price XLM.
func TestXLMSacContractID_MatchesDerivation(t *testing.T) {
	derived, err := NativeAsset().SacContractID()
	if err != nil {
		t.Fatalf("SacContractID(native): %v", err)
	}
	if derived != XLMSacContractID {
		t.Errorf("XLMSacContractID = %q, want the derivation %q", XLMSacContractID, derived)
	}
	// orientation.go's ranking must key off the same literal.
	if NativeSACContractID() != XLMSacContractID {
		t.Errorf("NativeSACContractID() = %q, want XLMSacContractID %q", NativeSACContractID(), XLMSacContractID)
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

// TestEURCAliasFamily_IsValid mirrors TestXLMAliasFamily_IsValid for
// the EURC/EUROC rename family: struct literals bypass the
// New* constructors, so validity and ordering are asserted here.
func TestEURCAliasFamily_IsValid(t *testing.T) {
	for _, a := range eurcAliasFamily {
		if err := a.Validate(); err != nil {
			t.Errorf("alias family member %q invalid: %v", a.String(), err)
		}
	}
	if len(eurcAliasFamily) != 2 {
		t.Fatalf("family size = %d, want 2 (EURC, EUROC)", len(eurcAliasFamily))
	}
	// EURC (the verified catalogue's post-rename ticker) is canonical;
	// EUROC folds onto it.
	want := []string{"crypto:EURC", "crypto:EUROC"}
	for i := range want {
		if got := eurcAliasFamily[i].String(); got != want[i] {
			t.Errorf("family[%d] = %q, want %q", i, got, want[i])
		}
	}
}

// TestAssetAliases_EURCEUROC: both spellings must resolve to
// the same two-member family regardless of which form a caller names,
// mirroring the XLM cases in TestAssetAliases.
func TestAssetAliases_EURCEUROC(t *testing.T) {
	cases := map[string][]string{
		"crypto:EURC":  {"crypto:EURC", "crypto:EUROC"},
		"crypto:EUROC": {"crypto:EUROC", "crypto:EURC"},
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

	// EUROB is a different token, deliberately not in the family.
	eurob, err := ParseAsset("crypto:EUROB")
	if err != nil {
		t.Fatalf("parse crypto:EUROB: %v", err)
	}
	if got := AssetAliases(eurob); len(got) != 1 || !got[0].Equal(eurob) {
		t.Errorf("AssetAliases(EUROB) = %v, want just itself", got)
	}
}

// TestAssetAliases is the contract test for the primitive that
// internal/api/v1 and internal/aggregate BOTH delegate to, so the
// two packages cannot drift apart.
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
// so a form set missing the SAC undercounts Soroban XLM volume.
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

// Pins the SQL-fold projection the volume-character rollup binds: every non-canonical form maps to its canonical form.
// The canonical form is not a key (the caller's COALESCE covers it). Baseline: {crypto:XLM -> native, <XLM SAC> ->
// native, crypto:EUROC -> crypto:EURC}.
func TestAllAliasForms(t *testing.T) {
	// Reset to the compile-time baseline so the test is independent of
	// any config-derived registry a prior test installed.
	InstallAliasRegistry(nil)

	got := AllAliasForms()
	want := map[string]string{
		"crypto:XLM":     "native",
		XLMSacContractID: "native",
		"crypto:EUROC":   "crypto:EURC",
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
	if _, ok := got["crypto:EURC"]; ok {
		t.Errorf("AllAliasForms() maps the canonical form 'crypto:EURC' — it must be omitted")
	}
}

// TestAliasForms_ConfigRegistry proves a configured classic↔SAC pair adds
// exactly its SAC→classic entry (SAC folds onto the classic, never the
// reverse), on top of the XLM baseline.
func TestAliasForms_ConfigRegistry(t *testing.T) {
	// USDC classic ↔ its SAC (the operator's canonical example).
	const usdcClassic = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	const usdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	reg, err := NewAliasRegistry(PubnetPassphrase, map[string]string{usdcSAC: usdcClassic})
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

// The SAC wrapper and its classic asset are the SAME asset, so a listing must not rank them as two rows. Canonical must
// map both forms to the classic (priority-first) member; echoing the input or Aliases()[0] would give different group
// keys.
func TestCanonical_FoldsSACOntoClassic(t *testing.T) {
	installTestRegistry(t, map[string]string{usdcSACAddr: "USDC:" + usdcIssuer})

	usdc, err := NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatalf("classic: %v", err)
	}
	sac, err := NewSorobanAsset(usdcSACAddr)
	if err != nil {
		t.Fatalf("soroban: %v", err)
	}

	// The SAC form folds onto the classic form — this is the load-bearing
	// direction: a SAC-keyed row must not be its own canonical group.
	if got := CanonicalAsset(sac); !got.Equal(usdc) {
		t.Errorf("CanonicalAsset(USDC SAC) = %q, want classic %q — the SAC twin must fold onto the classic row",
			got.String(), usdc.String())
	}
	// The classic form is already canonical — it is the surviving row.
	if got := CanonicalAsset(usdc); !got.Equal(usdc) {
		t.Errorf("CanonicalAsset(USDC classic) = %q, want itself", got.String())
	}
}

// TestCanonical_FoldsXLMFormsOntoNative pins the unconditional XLM baseline:
// crypto:XLM and the XLM SAC both fold onto `native`, independent of any
// configured wrapper (baseAliasFamilies). native is the priority-first form.
func TestCanonical_FoldsXLMFormsOntoNative(t *testing.T) {
	installTestRegistry(t, map[string]string{usdcSACAddr: "USDC:" + usdcIssuer})

	native := NativeAsset()
	xlmCrypto := Asset{Type: AssetCrypto, Code: "XLM"}
	xlmSAC := Asset{Type: AssetSoroban, ContractID: XLMSacContractID}

	for _, form := range []Asset{native, xlmCrypto, xlmSAC} {
		if got := CanonicalAsset(form); !got.Equal(native) {
			t.Errorf("CanonicalAsset(%q) = %q, want native", form.String(), got.String())
		}
	}
}

// TestCanonical_UnconfiguredIsItsOwnCanonical proves the fold never merges
// unrelated assets: an asset with no family is its own canonical group, so
// callers fold unconditionally without collapsing distinct rows.
func TestCanonical_UnconfiguredIsItsOwnCanonical(t *testing.T) {
	installTestRegistry(t, map[string]string{usdcSACAddr: "USDC:" + usdcIssuer})

	aqua, err := NewClassicAsset("AQUA", usdcIssuer)
	if err != nil {
		t.Fatalf("classic: %v", err)
	}
	if got := CanonicalAsset(aqua); !got.Equal(aqua) {
		t.Errorf("CanonicalAsset(unconfigured AQUA) = %q, want itself", got.String())
	}
}
