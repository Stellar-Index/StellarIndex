// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import "testing"

// TestCanonical_FoldsSACOntoClassic is the directory-fold key regression
// (task #28 Part A): the SAC wrapper form and its classic asset are the
// SAME asset, so a listing that ranks them as two rows shows "USDC twice".
// Canonical must map BOTH forms to the classic (priority-first) member so
// the fold groups them onto one row.
//
// RED without the Canonical fix: a naive resolver that echoes the input
// (or returns Aliases()[0], which leads with the caller's own spelling)
// returns the SAC form for a SAC-keyed call, so the two rows never share a
// group key and the duplicate survives.
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
