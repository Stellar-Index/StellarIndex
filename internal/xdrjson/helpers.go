package xdrjson

import (
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// amount renders a classic Int64 stroop amount as a decimal string (ADR-0003).
func amount(stroops int64) string { return strconv.FormatInt(stroops, 10) }

// muxedAddr renders an xdr.MuxedAccount as its strkey without panicking: Address() panics on an unknown key-type discriminant, so
// GetAddress is used and a malformed destination degrades to "" rather than 500-ing the transaction.
func muxedAddr(m xdr.MuxedAccount) string {
	s, _ := m.GetAddress()
	return s
}

// assetID renders an xdr.Asset as the canonical explorer id: "native" or
// "CODE-ISSUER" (dash form, matching the rest of the API).
func assetID(a xdr.Asset) string {
	switch a.Type {
	case xdr.AssetTypeAssetTypeNative:
		return "native"
	case xdr.AssetTypeAssetTypeCreditAlphanum4:
		an := a.MustAlphaNum4()
		code, ok := assetCode(an.AssetCode[:])
		if !ok {
			return "unknown_asset"
		}
		return code + "-" + an.Issuer.Address()
	case xdr.AssetTypeAssetTypeCreditAlphanum12:
		an := a.MustAlphaNum12()
		code, ok := assetCode(an.AssetCode[:])
		if !ok {
			return "unknown_asset"
		}
		return code + "-" + an.Issuer.Address()
	default:
		return "unknown_asset"
	}
}

// assetCode trims NUL padding from a fixed-width code and validates it with [canonical.ValidateAssetCode], the rule AssetFromXDR
// enforces; ok=false for a code it would refuse, so this package never mints an asset id the canonical layer disagrees with.
func assetCode(raw []byte) (string, bool) {
	code := strings.TrimRight(string(raw), "\x00")
	if canonical.ValidateAssetCode(code) != nil {
		return "", false
	}
	return code, true
}

// AssetID is the canonical asset id ("native" / "CODE-ISSUER") the API and explorer use, so lake rows can carry an API-consistent asset key.
func AssetID(a xdr.Asset) string { return assetID(a) }

// ClaimableBalanceIDHex renders a balance id the way the served entry fields do.
func ClaimableBalanceIDHex(id xdr.ClaimableBalanceId) (string, bool) {
	return claimableBalanceIDHex(id)
}

// TrustLineAssetID renders a trustline's asset as "CODE-ISSUER", "native", or "pool:<hex pool id>" (a distinct namespace so pool shares never collide with credit assets).
func TrustLineAssetID(t xdr.TrustLineAsset) string {
	switch t.Type {
	case xdr.AssetTypeAssetTypeNative:
		return "native"
	case xdr.AssetTypeAssetTypeCreditAlphanum4:
		an := t.MustAlphaNum4()
		code, ok := assetCode(an.AssetCode[:])
		if !ok {
			return "unknown_asset"
		}
		return code + "-" + an.Issuer.Address()
	case xdr.AssetTypeAssetTypeCreditAlphanum12:
		an := t.MustAlphaNum12()
		code, ok := assetCode(an.AssetCode[:])
		if !ok {
			return "unknown_asset"
		}
		return code + "-" + an.Issuer.Address()
	case xdr.AssetTypeAssetTypePoolShare:
		if t.LiquidityPoolId != nil {
			return "pool:" + hex.EncodeToString((*t.LiquidityPoolId)[:])
		}
		return "pool"
	default:
		return "unknown_asset"
	}
}

// assetPath maps a path-payment asset path to a slice of canonical ids.
func assetPath(path []xdr.Asset) []string {
	out := make([]string, len(path))
	for i, a := range path {
		out[i] = assetID(a)
	}
	return out
}

// changeTrustAsset renders a change-trust line: a regular asset, or a pool-share trustline as
// "pool:<hex>". That is the spelling TrustLineAssetID uses, derived from AssetA/AssetB/Fee with
// the SDK's pool-id hash, so a change_trust op and its trustline never disagree on the id.
func changeTrustAsset(c xdr.ChangeTrustAsset) string {
	switch c.Type {
	case xdr.AssetTypeAssetTypeNative:
		return "native"
	case xdr.AssetTypeAssetTypeCreditAlphanum4:
		an := c.MustAlphaNum4()
		code, ok := assetCode(an.AssetCode[:])
		if !ok {
			return "unknown_asset"
		}
		return code + "-" + an.Issuer.Address()
	case xdr.AssetTypeAssetTypeCreditAlphanum12:
		an := c.MustAlphaNum12()
		code, ok := assetCode(an.AssetCode[:])
		if !ok {
			return "unknown_asset"
		}
		return code + "-" + an.Issuer.Address()
	case xdr.AssetTypeAssetTypePoolShare:
		return poolShareLine(c.MustLiquidityPool())
	default:
		return "unknown_asset"
	}
}

// poolShareLine derives a change-trust pool-share line's "pool:<hex>" id from its asset parameters; "unknown_asset" is a forward-compat
// guard for a liquidity-pool type the SDK's NewPoolId doesn't recognise (none today).
func poolShareLine(params xdr.LiquidityPoolParameters) string {
	cp, ok := params.GetConstantProduct()
	if !ok {
		return "unknown_asset"
	}
	id, err := xdr.NewPoolId(cp.AssetA, cp.AssetB, cp.Fee)
	if err != nil {
		return "unknown_asset"
	}
	return "pool:" + hex.EncodeToString(id[:])
}

// claimableBalanceIDHex renders a ClaimableBalanceId's V0 hash as lowercase hex, matching the
// claimable_balances source. ok=false for a union arm without a V0 hash, refusing rather than
// guessing as assetCode does.
func claimableBalanceIDHex(id xdr.ClaimableBalanceId) (string, bool) {
	if id.V0 == nil {
		return "", false
	}
	return hex.EncodeToString((*id.V0)[:]), true
}

// poolIDHex renders a liquidity-pool id as lowercase hex — the same encoding
// TrustLineAssetID uses after its "pool:" prefix.
func poolIDHex(id xdr.PoolId) string { return hex.EncodeToString(id[:]) }

// price renders an xdr.Price (rational N/D) as a {n,d} object.
func price(p xdr.Price) map[string]any {
	return map[string]any{"n": int32(p.N), "d": int32(p.D)}
}

// base64Bytes base64-encodes a raw byte slice (e.g. a ManageData value).
func base64Bytes(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// contractAddress renders an ScAddress as a strkey for the account/contract
// cases (the targets an InvokeContract can carry). ok=false for other shapes.
func contractAddress(addr xdr.ScAddress) (string, bool) {
	switch addr.Type {
	case xdr.ScAddressTypeScAddressTypeContract:
		raw := addr.MustContractId()
		s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
		if err != nil {
			return "", false
		}
		return s, true
	case xdr.ScAddressTypeScAddressTypeAccount:
		raw := addr.MustAccountId().Ed25519
		s, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
		if err != nil {
			return "", false
		}
		return s, true
	default:
		return "", false
	}
}

// MemoTypeName normalises the lake's XDR memo-type enum ("MemoTypeMemoText") to the wire value ("text"); "none" for empty.
func MemoTypeName(xdrEnum string) string {
	s := strings.TrimPrefix(xdrEnum, "MemoTypeMemo")
	if s == "" {
		return "none"
	}
	return strings.ToLower(s)
}
