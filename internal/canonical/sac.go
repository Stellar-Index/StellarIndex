// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"fmt"
	"strings"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// PubnetPassphrase is the Stellar mainnet network passphrase — the
// input to deterministic SAC contract-ID derivation. Kept here (not
// config) because the canonical asset model is pubnet-scoped
// throughout (docs/architecture/coverage-matrix.md).
const PubnetPassphrase = "Public Global Stellar Network ; September 2015"

// TestnetPassphrase and FuturenetPassphrase are the two SDF test
// networks' passphrases (the values config.StellarConfig.Passphrase()
// resolves "testnet" / "futurenet" to). Exported here so leaf packages
// that key network-dependent constants off the passphrase (e.g. the
// native-XLM total in internal/supply) can compare against one source
// instead of re-spelling the literals.
const (
	TestnetPassphrase   = "Test SDF Network ; September 2015"
	FuturenetPassphrase = "Test SDF Future Network ; October 2022"
)

// SacContractID returns the C-strkey of the asset's Stellar Asset
// Contract on the CONFIGURED network ([NetworkPassphrase] — pubnet
// unless a binary installed another at start-up via
// [InstallNetworkPassphrase]). The SAC address is a pure function of
// (asset, network passphrase) — no on-chain lookup, and it is valid
// even for assets whose SAC has never been deployed (deployment is
// permissionless and address-stable, so wallets treat the derived
// address as THE address). Deriving against the wrong network would
// serve a contract address that resolves to nothing / a different
// asset on the target network — see InstallNetworkPassphrase.
//
// Board #40 (RFP audit): both RFPs put "Contract Address" in the
// asset-metadata table for classic assets; wallets resolve holdings
// by contract address post-Soroban, so the classic detail must carry
// this and a C-address lookup must land on the classic identity.
//
// Returns an error only for asset shapes with no SAC (a Soroban
// token IS its own contract; use Asset.ContractID directly).
func (a Asset) SacContractID() (string, error) {
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
	raw, err := x.ContractID(NetworkPassphrase())
	if err != nil {
		return "", fmt.Errorf("canonical: SacContractID: derive: %w", err)
	}
	return strkey.MustEncode(strkey.VersionByteContract, raw[:]), nil
}

// SEP11SACAsset resolves a SEP-0011 asset name ("native" or "CODE:GISSUER",
// as a CAP-67 event's trailing topic carries it) to the classic asset ONLY
// when contractID is that asset's SAC on the configured network. Any
// contract can emit that topic, so the name alone is never an identity; the
// deterministic derivation is. ok=false means the claim must not be trusted.
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
