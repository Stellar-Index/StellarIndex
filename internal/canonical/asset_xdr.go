// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// AssetFromXDR converts an [xdr.Asset] to its canonical form, the ONE definition of which on-chain assets we represent; it errors rather than return a partial asset for anything not representable exactly.
func AssetFromXDR(a xdr.Asset) (Asset, error) {
	switch a.Type {
	case xdr.AssetTypeAssetTypeNative:
		return NativeAsset(), nil
	case xdr.AssetTypeAssetTypeCreditAlphanum4:
		a4 := a.MustAlphaNum4()
		code := TrimTrailingNulls(a4.AssetCode[:])
		issuer, err := strkey.Encode(strkey.VersionByteAccountID, a4.Issuer.Ed25519[:])
		if err != nil {
			return Asset{}, fmt.Errorf("canonical: AssetFromXDR: alphanum4 issuer: %w", err)
		}
		return NewClassicAsset(code, issuer)
	case xdr.AssetTypeAssetTypeCreditAlphanum12:
		a12 := a.MustAlphaNum12()
		code := TrimTrailingNulls(a12.AssetCode[:])
		issuer, err := strkey.Encode(strkey.VersionByteAccountID, a12.Issuer.Ed25519[:])
		if err != nil {
			return Asset{}, fmt.Errorf("canonical: AssetFromXDR: alphanum12 issuer: %w", err)
		}
		return NewClassicAsset(code, issuer)
	}
	return Asset{}, fmt.Errorf("canonical: AssetFromXDR: unsupported asset type %d", a.Type)
}

// TrimTrailingNulls removes only trailing zero padding from a classic asset-code array, so an interior null still fails [validateClassicAssetCode].
func TrimTrailingNulls(b []byte) string {
	n := len(b)
	for n > 0 && b[n-1] == 0 {
		n--
	}
	return string(b[:n])
}
