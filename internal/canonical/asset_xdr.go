// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// AssetFromXDR converts an [xdr.Asset] to its canonical form. It is the
// ONE definition of "which on-chain assets this system can represent",
// and therefore of which SDEX fills become trade rows.
//
// Handles the three classic asset variants: native, credit alphanum4,
// credit alphanum12. A Soroban SAC-wrapped classic asset arrives here as
// classic — the SAC address is metadata, not canonical identity (see the
// [Asset] docstring and [Asset.SacContractID]).
//
// It returns an ERROR — never a partial or best-effort asset — for every
// input the model cannot represent exactly:
//
//   - an unsupported/future xdr.AssetType;
//   - an issuer whose ed25519 bytes don't strkey-encode;
//   - a code that fails [validateClassicAssetCode] (empty after
//     null-trim, over 12 bytes, or carrying a non-ASCII-alphanumeric
//     byte — stellar-core does not enforce the character rule, so
//     control-byte and emoji codes DO occur on pubnet).
//
// It lives in this leaf package, not unexported in internal/sources/sdex,
// because the dispatcher census and the ClickHouse lake extractor count
// "real trades" from the same ClaimAtom slices as the decoder. Without
// the shared rule every fill whose asset code the decoder rejected would
// be counted by the oracles and dropped by the writer — a permanent,
// unresolvable reconcile discrepancy that looks like real trade loss.
// The leaf package lets internal/sdexclaim apply the identical rule (it
// cannot import sdex: that cycles back through internal/dispatcher).
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

// TrimTrailingNulls removes zero bytes from the end of a classic
// asset-code byte slice. AssetCode arrays are fixed-size (4 or 12 bytes)
// with nulls padding short codes ("AQUA" in a 12-array has 8 nulls).
//
// It trims ONLY trailing nulls: an interior null (or any other control
// byte) is left in place so [validateClassicAssetCode] can reject the
// code, rather than being silently truncated into a different — and
// possibly colliding — asset identity.
func TrimTrailingNulls(b []byte) string {
	n := len(b)
	for n > 0 && b[n-1] == 0 {
		n--
	}
	return string(b[:n])
}
