package xdrjson

import (
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SACContractID returns the C-strkey of the Stellar Asset Contract for a classic/native asset on the given network (deterministic via xdr.Asset.ContractID); ok=false for assets with no classic SAC or on encode error.
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
