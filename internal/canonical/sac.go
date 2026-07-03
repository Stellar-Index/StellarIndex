// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// PubnetPassphrase is the Stellar mainnet network passphrase — the
// input to deterministic SAC contract-ID derivation. Kept here (not
// config) because the canonical asset model is pubnet-scoped
// throughout (docs/architecture/coverage-matrix.md).
const PubnetPassphrase = "Public Global Stellar Network ; September 2015"

// SacContractID returns the C-strkey of the asset's Stellar Asset
// Contract on pubnet. The SAC address is a pure function of
// (asset, network passphrase) — no on-chain lookup, and it is valid
// even for assets whose SAC has never been deployed (deployment is
// permissionless and address-stable, so wallets treat the derived
// address as THE address).
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
	raw, err := x.ContractID(PubnetPassphrase)
	if err != nil {
		return "", fmt.Errorf("canonical: SacContractID: derive: %w", err)
	}
	return strkey.MustEncode(strkey.VersionByteContract, raw[:]), nil
}
