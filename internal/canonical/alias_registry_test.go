// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Circle's live USDC issuer and its pubnet Stellar Asset Contract, as
// configured in the archival-node sac_wrappers template.
const (
	usdcIssuer  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdcSACAddr = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	// A valid C-strkey that is NOT USDC's SAC.
	unrelatedContract = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
)

// installTestRegistry builds a registry from wrappers, installs it as the
// process registry, and resets to the XLM-only default on cleanup so the
// mutation never leaks to another test.
func installTestRegistry(t *testing.T, wrappers map[string]string) {
	t.Helper()
	reg, err := NewAliasRegistry(PubnetPassphrase, wrappers)
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
	if _, err := NewAliasRegistry(PubnetPassphrase, map[string]string{"not-a-contract-id": "USDC:" + usdcIssuer}); err == nil {
		t.Error("NewAliasRegistry accepted a malformed contract id, want error")
	}
	if _, err := NewAliasRegistry(PubnetPassphrase, map[string]string{usdcSACAddr: "not a valid asset key"}); err == nil {
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

// TestNewAliasRegistry_RejectsMispairedSAC: a wrapper whose contract is
// not the derived SAC of its asset would alias an arbitrary contract onto
// the classic identity (a scam-gate bypass), so it must fail the build.
func TestNewAliasRegistry_RejectsMispairedSAC(t *testing.T) {
	_, err := NewAliasRegistry(PubnetPassphrase, map[string]string{unrelatedContract: "USDC:" + usdcIssuer})
	if err == nil || !strings.Contains(err.Error(), "is not the SAC of") {
		t.Fatalf("NewAliasRegistry(mis-paired) err = %v, want derivation mismatch", err)
	}
	// Same pairing is only valid on the network it was derived for.
	if _, err := NewAliasRegistry(TestnetPassphrase, map[string]string{usdcSACAddr: "USDC:" + usdcIssuer}); err == nil {
		t.Error("NewAliasRegistry accepted a pubnet SAC id under the testnet passphrase, want error")
	}
	// A SEP-41 contract mapped onto a different contract has no SAC.
	if _, err := NewAliasRegistry(PubnetPassphrase, map[string]string{unrelatedContract: usdcSACAddr}); err == nil {
		t.Error("NewAliasRegistry accepted contract→other-contract, want error")
	}
}

// TestNewAliasRegistry_RejectsDuplicateClassic: a second contract claiming
// an already-wrapped classic asset is an error, not a silent skip.
func TestNewAliasRegistry_RejectsDuplicateClassic(t *testing.T) {
	_, err := NewAliasRegistry(PubnetPassphrase, map[string]string{
		usdcSACAddr:       "USDC:" + usdcIssuer,
		unrelatedContract: "USDC-" + usdcIssuer,
	})
	if err == nil {
		t.Fatal("NewAliasRegistry accepted two contracts for one classic asset, want error")
	}
}

// TestNewAliasRegistry_AcceptsConfiguredWrappers: the archival-node
// template's [supply.sac_wrappers], as rendered for each network the
// inventories deploy, builds a registry on that network's passphrase, so the
// deployed config boots everywhere. Pubnet loads every pair; a test net must
// render none of the pubnet contract ids, which cannot derive there.
func TestNewAliasRegistry_AcceptsConfiguredWrappers(t *testing.T) {
	for _, tc := range []struct {
		network, passphrase string
	}{
		{"pubnet", PubnetPassphrase},
		{"testnet", TestnetPassphrase},
		{"futurenet", FuturenetPassphrase},
	} {
		t.Run(tc.network, func(t *testing.T) {
			wrappers, total := templateSACWrappers(t, tc.network)
			if total < 3 {
				t.Fatalf("template carries %d sac_wrappers entries, want several", total)
			}
			if tc.network != "pubnet" {
				if len(wrappers) != 0 {
					t.Errorf("%s renders %d sac_wrappers entries, want none", tc.network, len(wrappers))
				}
				if _, err := NewAliasRegistry(tc.passphrase, wrappers); err != nil {
					t.Fatalf("NewAliasRegistry(%s): %v", tc.network, err)
				}
				return
			}

			if len(wrappers) != total || wrappers[usdcSACAddr] != "USDC:"+usdcIssuer {
				t.Fatalf("pubnet renders %d of %d sac_wrappers entries, want all incl. USDC", len(wrappers), total)
			}
			wrappers[XLMSacContractID] = "native"
			reg, err := NewAliasRegistry(tc.passphrase, wrappers)
			if err != nil {
				t.Fatalf("NewAliasRegistry(pubnet): %v", err)
			}
			for sac, key := range wrappers {
				if key == "native" {
					continue
				}
				code, issuer, _ := strings.Cut(key, ":")
				classic, err := NewClassicAsset(code, issuer)
				if err != nil {
					t.Fatalf("classic %q: %v", key, err)
				}
				if got := forms(reg.Aliases(classic)); len(got) != 2 || got[1] != sac {
					t.Errorf("Aliases(%s) = %v, want [%s, %s]", key, got, key, sac)
				}
			}
			if got := reg.Aliases(NativeAsset()); len(got) != len(xlmAliasFamily) {
				t.Errorf("Aliases(native) = %v, want the XLM baseline family", forms(got))
			}
		})
	}
}

// templateSACWrappers evaluates the template's [supply.sac_wrappers] table
// for one stellar_network, returning the rendered pairs and the count of
// entries in the table regardless of guard. Only the `{% if stellar_network
// == "..." %}` / `{% endif %}` guard is understood; any other jinja tag in
// the table fails the test rather than being evaluated wrongly.
func templateSACWrappers(t *testing.T, network string) (map[string]string, int) {
	t.Helper()
	const tmpl = "../../configs/ansible/roles/archival-node/templates/stellarindex.toml.j2"
	raw, err := os.ReadFile(tmpl)
	if err != nil {
		t.Fatalf("read %s: %v", tmpl, err)
	}
	_, section, ok := strings.Cut(string(raw), "[supply.sac_wrappers]\n")
	if !ok {
		t.Fatalf("%s: no [supply.sac_wrappers] table", tmpl)
	}
	entry := regexp.MustCompile(`^"(C[A-Z2-7]{55})"\s*=\s*"([^"]+)"$`)
	guard := regexp.MustCompile(`^\{%-?\s*if\s+stellar_network\s*==\s*["']([a-z]+)["']\s*-?%\}$`)
	endif := regexp.MustCompile(`^\{%-?\s*endif\s*-?%\}$`)
	wrappers := map[string]string{}
	total, active, inGuard := 0, true, false
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			if inGuard {
				t.Fatalf("%s: blank line inside a sac_wrappers guard", tmpl)
			}
			return wrappers, total
		case guard.MatchString(line):
			if inGuard {
				t.Fatalf("%s: nested sac_wrappers guard", tmpl)
			}
			inGuard, active = true, guard.FindStringSubmatch(line)[1] == network
		case endif.MatchString(line):
			inGuard, active = false, true
		case strings.Contains(line, "{%") || strings.Contains(line, "{{"):
			t.Fatalf("%s: unsupported jinja in sac_wrappers: %q", tmpl, line)
		default:
			m := entry.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("%s: unparsed sac_wrappers line %q", tmpl, line)
			}
			total++
			if active {
				wrappers[m[1]] = m[2]
			}
		}
	}
	t.Fatalf("%s: sac_wrappers table runs to EOF", tmpl)
	return nil, 0
}

func forms(as []Asset) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.String()
	}
	return out
}
