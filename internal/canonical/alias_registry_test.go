// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import "testing"

// A realistic USDC classic form and a (distinct, valid) C-strkey standing
// in for its Stellar Asset Contract wrapper. The issuer is Circle's live
// USDC issuer (already used by TestAssetAliases); the contract is a valid
// C-strkey distinct from the XLM SAC, so the family is keyed on the exact
// address, never on "is a C-address".
const (
	usdcIssuer  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdcSACAddr = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
)

// installTestRegistry builds a registry from wrappers, installs it as the
// process registry, and resets to the XLM-only default on cleanup so the
// mutation never leaks to another test.
func installTestRegistry(t *testing.T, wrappers map[string]string) {
	t.Helper()
	reg, err := NewAliasRegistry(wrappers)
	if err != nil {
		t.Fatalf("NewAliasRegistry(%v): %v", wrappers, err)
	}
	InstallAliasRegistry(reg)
	t.Cleanup(func() { InstallAliasRegistry(nil) })
}

// TestAliasRegistry_ClassicKeyedReadFoldsSAC is the core W2 regression:
// before the registry, a classic-form USDC read returned ONLY the classic
// form, so ~53.5% of USDC volume (the SAC-form rows Soroban AMMs write)
// was invisible on every served money path. With the configured wrapper
// installed, the classic-keyed read now folds in the SAC form too.
//
// RED without the fix: AssetAliases(USDC-classic) is length 1.
func TestAliasRegistry_ClassicKeyedReadFoldsSAC(t *testing.T) {
	installTestRegistry(t, map[string]string{usdcSACAddr: "USDC:" + usdcIssuer})

	usdc, err := NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatalf("classic: %v", err)
	}

	got := AssetAliases(usdc)
	want := []string{usdc.String(), usdcSACAddr}
	if len(got) != len(want) {
		t.Fatalf("AssetAliases(USDC classic) = %v (len %d), want %v (len %d) — SAC form must be folded in",
			forms(got), len(got), want, len(want))
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("AssetAliases(USDC classic)[%d] = %q, want %q", i, got[i].String(), want[i])
		}
	}

	// The string projection SQL binds must agree, form-for-form.
	gotStrs := AssetAliasStrings(usdc)
	if len(gotStrs) != len(want) {
		t.Fatalf("AssetAliasStrings(USDC classic) = %v, want %v", gotStrs, want)
	}
	for i := range want {
		if gotStrs[i] != want[i] {
			t.Errorf("AssetAliasStrings(USDC classic)[%d] = %q, want %q", i, gotStrs[i], want[i])
		}
	}
}

// TestAliasRegistry_SACOrderedLast pins the money-safety invariant: on a
// classic-keyed read the SAC form is the LAST alias tried, so a thin
// Soroban pool can never become the served price/volume ahead of the deep
// classic form. (A SAC-keyed read still returns the SAC literal first,
// because that caller explicitly asked for that form — mirroring the
// documented XLM rule.)
func TestAliasRegistry_SACOrderedLast(t *testing.T) {
	installTestRegistry(t, map[string]string{usdcSACAddr: "USDC:" + usdcIssuer})

	usdc, err := NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatalf("classic: %v", err)
	}
	classicKeyed := AssetAliases(usdc)
	if n := len(classicKeyed); n == 0 || classicKeyed[n-1].String() != usdcSACAddr {
		t.Fatalf("classic-keyed read = %v, want the SAC form %q LAST", forms(classicKeyed), usdcSACAddr)
	}
	if classicKeyed[0].String() != usdc.String() {
		t.Errorf("classic-keyed read must lead with the classic form, got %q", classicKeyed[0].String())
	}

	// SAC-keyed read: literal (SAC) first, classic fallback second.
	sac, err := NewSorobanAsset(usdcSACAddr)
	if err != nil {
		t.Fatalf("soroban: %v", err)
	}
	sacKeyed := AssetAliases(sac)
	wantSAC := []string{usdcSACAddr, usdc.String()}
	if len(sacKeyed) != len(wantSAC) || sacKeyed[0].String() != wantSAC[0] || sacKeyed[1].String() != wantSAC[1] {
		t.Errorf("SAC-keyed read = %v, want %v (literal first)", forms(sacKeyed), wantSAC)
	}
}

// TestAliasRegistry_XLMBaselineUnclobbered proves that installing a
// config registry NEVER weakens XLM's unconditional three-form family,
// and that an unconfigured asset still returns only itself.
func TestAliasRegistry_XLMBaselineUnclobbered(t *testing.T) {
	installTestRegistry(t, map[string]string{usdcSACAddr: "USDC:" + usdcIssuer})

	gotXLM := AssetAliasStrings(NativeAsset())
	wantXLM := []string{"native", "crypto:XLM", XLMSacContractID}
	if len(gotXLM) != len(wantXLM) {
		t.Fatalf("AssetAliasStrings(native) = %v, want %v", gotXLM, wantXLM)
	}
	for i := range wantXLM {
		if gotXLM[i] != wantXLM[i] {
			t.Errorf("XLM family[%d] = %q, want %q", i, gotXLM[i], wantXLM[i])
		}
	}

	// An asset with no configured wrapper is still a singleton.
	aqua, err := NewClassicAsset("AQUA", usdcIssuer)
	if err != nil {
		t.Fatalf("classic: %v", err)
	}
	if got := AssetAliases(aqua); len(got) != 1 || !got[0].Equal(aqua) {
		t.Errorf("AssetAliases(unconfigured) = %v, want just itself", forms(got))
	}
}

// TestNewAliasRegistry_FailsClosedOnMalformed pins the fail-closed
// contract: a malformed contract id or asset key is an error, never a
// silently dropped wrapper (a dropped wrapper is invisible under-counted
// volume — the exact defect the registry removes).
func TestNewAliasRegistry_FailsClosedOnMalformed(t *testing.T) {
	if _, err := NewAliasRegistry(map[string]string{"not-a-contract-id": "USDC:" + usdcIssuer}); err == nil {
		t.Error("NewAliasRegistry accepted a malformed contract id, want error")
	}
	if _, err := NewAliasRegistry(map[string]string{usdcSACAddr: "not a valid asset key"}); err == nil {
		t.Error("NewAliasRegistry accepted a malformed asset key, want error")
	}
}

// TestNewAliasRegistry_SkipsSelfMap covers the pure-SEP-41 convention
// (contract_id → contract_id): one identity, nothing to alias.
func TestNewAliasRegistry_SkipsSelfMap(t *testing.T) {
	installTestRegistry(t, map[string]string{usdcSACAddr: usdcSACAddr})
	sac, err := NewSorobanAsset(usdcSACAddr)
	if err != nil {
		t.Fatalf("soroban: %v", err)
	}
	if got := AssetAliases(sac); len(got) != 1 || !got[0].Equal(sac) {
		t.Errorf("self-mapped SAC = %v, want just itself", forms(got))
	}
}

func forms(as []Asset) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.String()
	}
	return out
}
