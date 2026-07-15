package xdrjson

import (
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SACContractID returns the Stellar Asset Contract (SAC) contract id —
// as a C-strkey — for a canonical classic/native asset on the given
// network. The SAC id is deterministic from the asset + network
// passphrase (xdr.Asset.ContractID), so this lets callers map a SAC
// contract address back to the classic asset it wraps (e.g. to price a
// Blend reserve, whose underlying is the asset's SAC).
//
// ok=false for assets with no classic SAC (fiat / pure-crypto / a
// Soroban-native token that isn't a SAC wrapper) or on any encode error.
func SACContractID(assetID, passphrase string) (string, bool) {
	ca, err := canonical.ParseAsset(assetID)
	if err != nil {
		return "", false
	}
	var a xdr.Asset
	switch ca.Type {
	case canonical.AssetNative:
		a = xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	case canonical.AssetClassic:
		var issuer xdr.AccountId
		if err := issuer.SetAddress(ca.Issuer); err != nil {
			return "", false
		}
		if len(ca.Code) <= 4 {
			var code [4]byte
			copy(code[:], ca.Code)
			a = xdr.Asset{
				Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
				AlphaNum4: &xdr.AlphaNum4{AssetCode: code, Issuer: issuer},
			}
		} else {
			var code [12]byte
			copy(code[:], ca.Code)
			a = xdr.Asset{
				Type:       xdr.AssetTypeAssetTypeCreditAlphanum12,
				AlphaNum12: &xdr.AlphaNum12{AssetCode: code, Issuer: issuer},
			}
		}
	default:
		return "", false
	}
	id, err := a.ContractID(passphrase)
	if err != nil {
		return "", false
	}
	c, err := strkey.Encode(strkey.VersionByteContract, id[:])
	if err != nil {
		return "", false
	}
	return c, true
}
