// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical_test

import (
	"slices"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

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

// testnetNativeSAC is the well-known testnet XLM Stellar Asset Contract.
const testnetNativeSAC = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"

func TestNativeSACContractID_PubnetUnchangedAndNetworkAware(t *testing.T) {
	t.Cleanup(func() { canonical.InstallNetworkPassphrase("") })

	canonical.InstallNetworkPassphrase("")
	if got := canonical.NativeSACContractID(); got != "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA" {
		t.Errorf("uninstalled NativeSACContractID() = %q, want the pre-change pubnet literal", got)
	}
	canonical.InstallNetworkPassphrase(canonical.PubnetPassphrase)
	if got := canonical.NativeSACContractID(); got != canonical.XLMSacContractID {
		t.Errorf("pubnet NativeSACContractID() = %q, want %q", got, canonical.XLMSacContractID)
	}

	canonical.InstallNetworkPassphrase(testnetPassphrase)
	want, err := canonical.NativeAsset().SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	got := canonical.NativeSACContractID()
	if got != want || got != testnetNativeSAC {
		t.Errorf("testnet NativeSACContractID() = %q, SDK derivation %q, known %q", got, want, testnetNativeSAC)
	}
}

func TestInstallNetwork_TestnetAliasFamilyUsesTestnetSAC(t *testing.T) {
	t.Cleanup(func() {
		canonical.InstallNetworkPassphrase("")
		canonical.InstallAliasRegistry(nil)
	})
	if err := canonical.InstallNetwork(testnetPassphrase, nil); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range canonical.AssetAliases(canonical.NativeAsset()) {
		got = append(got, a.String())
	}
	want := []string{"native", "crypto:XLM", testnetNativeSAC}
	if len(got) != len(want) {
		t.Fatalf("testnet XLM family = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("testnet XLM family = %v, want %v", got, want)
			break
		}
	}
	for _, s := range got {
		if s == canonical.XLMSacContractID {
			t.Errorf("testnet XLM family contains the pubnet SAC: %v", got)
		}
	}
}

// TestInstallNetworkPassphrase_AliasBaselineFollowsNetwork: with only the
// passphrase installed (no config registry), every alias projection the SQL
// read paths bind must carry the testnet XLM SAC, never the pubnet one.
func TestInstallNetworkPassphrase_AliasBaselineFollowsNetwork(t *testing.T) {
	t.Cleanup(func() { canonical.InstallNetworkPassphrase("") })
	canonical.InstallAliasRegistry(nil)
	canonical.InstallNetworkPassphrase(testnetPassphrase)

	raw, err := xdr.MustNewNativeAsset().ContractID(testnetPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	sac := strkey.MustEncode(strkey.VersionByteContract, raw[:])
	if sac == canonical.XLMSacContractID {
		t.Fatal("SDK derived the pubnet SAC for the testnet passphrase")
	}

	want := []string{"native", "crypto:XLM", sac}
	if got := canonical.AssetAliasStrings(canonical.NativeAsset()); !slices.Equal(got, want) {
		t.Errorf("AssetAliasStrings(native) = %v, want %v", got, want)
	}
	var nilReg *canonical.AliasRegistry
	if got := nilReg.AliasStrings(canonical.NativeAsset()); !slices.Equal(got, want) {
		t.Errorf("nil registry AliasStrings(native) = %v, want %v", got, want)
	}
	if got := canonical.CanonicalAsset(canonical.Asset{Type: canonical.AssetSoroban, ContractID: sac}); got.String() != "native" {
		t.Errorf("CanonicalAsset(testnet SAC) = %s, want native", got)
	}
	forms := canonical.AllAliasForms()
	if forms[sac] != "native" {
		t.Errorf("AllAliasForms()[testnet SAC] = %q, want native", forms[sac])
	}
	if _, ok := forms[canonical.XLMSacContractID]; ok {
		t.Errorf("AllAliasForms() still folds the pubnet SAC on testnet: %v", forms)
	}
	if got := canonical.NativeSACContractID(); got != sac {
		t.Errorf("NativeSACContractID() = %q, want %q", got, sac)
	}
}
