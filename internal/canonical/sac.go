// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"fmt"
	"strings"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// PubnetPassphrase is the Stellar mainnet passphrase, the input to deterministic SAC
// contract-ID derivation. Kept here, not in config, because the asset model is pubnet-scoped.
const PubnetPassphrase = "Public Global Stellar Network ; September 2015"

// TestnetPassphrase and FuturenetPassphrase are the SDF test networks' passphrases, exported so
// leaf packages keying network-dependent constants off the passphrase share one source.
const (
	TestnetPassphrase   = "Test SDF Network ; September 2015"
	FuturenetPassphrase = "Test SDF Future Network ; October 2022"
)

// SacContractID returns the C-strkey of the asset's Stellar Asset Contract on the configured
// network ([NetworkPassphrase]; see [InstallNetworkPassphrase]). It is a pure function of
// (asset, passphrase) with no on-chain lookup, valid even if the SAC was never deployed
// (deployment is permissionless and address-stable). The wrong network would yield an address
// resolving to nothing or a different asset.
//
// Wallets resolve holdings by contract address, so classic detail must carry this and a
// C-address lookup must land on the classic identity.
//
// It errors only for shapes with no SAC (a Soroban token is its own contract; use ContractID).
func (a Asset) SacContractID() (string, error) {
	return a.sacContractIDOn(NetworkPassphrase())
}

// sacContractIDOn is [Asset.SacContractID] against an explicit network
// passphrase, for callers that hold the configured network directly.
func (a Asset) sacContractIDOn(passphrase string) (string, error) {
	var x xdr.Asset
	switch a.Type {
	case AssetNative:
		x = xdr.MustNewNativeAsset()
	case AssetClassic:
		var err error
		x, err = xdr.NewCreditAsset(a.Code, a.Issuer)
		if err != nil {
			return "", fmt.Errorf("canonical: SacContractID(%s-%s): %w", a.Code, a.Issuer, err)
		}
	default:
		return "", fmt.Errorf("canonical: SacContractID: asset type %q has no SAC (use ContractID)", a.Type)
	}
	raw, err := x.ContractID(passphrase)
	if err != nil {
		return "", fmt.Errorf("canonical: SacContractID: derive: %w", err)
	}
	return strkey.MustEncode(strkey.VersionByteContract, raw[:]), nil
}

// ContractAddress is the asset's C-address: a Soroban token's own contract, or the derived SAC for
// classic and native. ok=false for shapes with neither (fiat, external, raw).
func (a Asset) ContractAddress() (string, bool) {
	if a.ContractID != "" {
		return a.ContractID, true
	}
	if a.Type != AssetClassic && a.Type != AssetNative {
		return "", false
	}
	sac, err := a.SacContractID()
	return sac, err == nil
}

// SEP11SACAsset resolves a SEP-0011 name ("native" or "CODE:GISSUER", a CAP-67 event's trailing
// topic) to the classic asset ONLY when contractID is its SAC on the configured network. Any
// contract can emit that topic, so the name is never an identity; the derivation is.
// ok=false means the claim must not be trusted.
func SEP11SACAsset(name, contractID string) (Asset, bool) {
	var asset Asset
	if name == "native" {
		asset = NativeAsset()
	} else {
		code, issuer, ok := strings.Cut(name, ":")
		if !ok {
			return Asset{}, false
		}
		var err error
		if asset, err = NewClassicAsset(code, issuer); err != nil {
			return Asset{}, false
		}
	}
	derived, err := asset.SacContractID()
	if err != nil || derived != contractID {
		return Asset{}, false
	}
	return asset, true
}
