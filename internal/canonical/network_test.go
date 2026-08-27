// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical_test

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const testnetPassphrase = "Test SDF Network ; September 2015"

// TestNetworkPassphrase_DefaultsToPubnet: uninstalled resolves to pubnet,
// so binaries that never install (and every unit test) keep pubnet
// behaviour.
func TestNetworkPassphrase_DefaultsToPubnet(t *testing.T) {
	canonical.InstallNetworkPassphrase("") // reset
	if got := canonical.NetworkPassphrase(); got != canonical.PubnetPassphrase {
		t.Errorf("NetworkPassphrase() default = %q, want PubnetPassphrase", got)
	}
}

// TestNetworkPassphrase_InstallAndReset: install overrides; empty resets.
func TestNetworkPassphrase_InstallAndReset(t *testing.T) {
	t.Cleanup(func() { canonical.InstallNetworkPassphrase("") })
	canonical.InstallNetworkPassphrase(testnetPassphrase)
	if got := canonical.NetworkPassphrase(); got != testnetPassphrase {
		t.Errorf("NetworkPassphrase() = %q, want %q", got, testnetPassphrase)
	}
	canonical.InstallNetworkPassphrase("")
	if got := canonical.NetworkPassphrase(); got != canonical.PubnetPassphrase {
		t.Errorf("NetworkPassphrase() after reset = %q, want PubnetPassphrase", got)
	}
}

// TestSacContractID_NetworkAware is the corruption-fix proof: the SAC
// address a classic/native asset resolves to is a pure function of the
// network passphrase, so it DIFFERS by network. Before the fix
// SacContractID always used the pubnet passphrase, so a testnet
// /v1/assets/{id} served the PUBNET contract address — a value wallets
// resolve holdings against and send to. This test fails if SacContractID
// ever ignores the installed network again.
func TestSacContractID_NetworkAware(t *testing.T) {
	t.Cleanup(func() { canonical.InstallNetworkPassphrase("") })

	canonical.InstallNetworkPassphrase("") // pubnet
	pub, err := canonical.NativeAsset().SacContractID()
	if err != nil {
		t.Fatalf("pubnet SacContractID: %v", err)
	}
	if pub != canonical.XLMSacContractID {
		t.Errorf("pubnet native SAC = %q, want the pinned pubnet const %q", pub, canonical.XLMSacContractID)
	}

	canonical.InstallNetworkPassphrase(testnetPassphrase)
	test, err := canonical.NativeAsset().SacContractID()
	if err != nil {
		t.Fatalf("testnet SacContractID: %v", err)
	}
	if test == pub {
		t.Errorf("testnet native SAC == pubnet SAC (%q) — SacContractID is not network-aware; a testnet asset detail would serve the pubnet contract address", pub)
	}
}
