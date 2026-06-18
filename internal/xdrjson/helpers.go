package xdrjson

import (
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// amount renders a classic Int64 stroop amount as a decimal string (ADR-0003).
func amount(stroops int64) string { return strconv.FormatInt(stroops, 10) }

// muxedAddr renders an xdr.MuxedAccount as its strkey WITHOUT panicking.
// MuxedAccount.Address() panics on an unknown key-type discriminant; GetAddress
// returns ("", false) instead, so a single malformed destination degrades to an
// empty string rather than 500-ing the whole transaction response.
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
		return assetCode(an.AssetCode[:]) + "-" + an.Issuer.Address()
	case xdr.AssetTypeAssetTypeCreditAlphanum12:
		an := a.MustAlphaNum12()
		return assetCode(an.AssetCode[:]) + "-" + an.Issuer.Address()
	default:
		return "unknown_asset"
	}
}

// assetCode trims the trailing NUL padding from a fixed-width asset code.
func assetCode(raw []byte) string {
	return strings.TrimRight(string(raw), "\x00")
}

// AssetID is the exported canonical asset id ("native" / "CODE-ISSUER") —
// the same form the rest of the API + explorer use. Lets other packages
// (e.g. the lake entry-change extractor) tag rows with a queryable,
// API-consistent asset key.
func AssetID(a xdr.Asset) string { return assetID(a) }

// TrustLineAssetID renders a trustline entry's asset in the canonical
// explorer form. Regular credits → "CODE-ISSUER"; native → "native";
// liquidity-pool-share trustlines → "pool:<hex pool id>" (a distinct
// namespace so they never collide with a credit-asset key).
func TrustLineAssetID(t xdr.TrustLineAsset) string {
	switch t.Type {
	case xdr.AssetTypeAssetTypeNative:
		return "native"
	case xdr.AssetTypeAssetTypeCreditAlphanum4:
		an := t.MustAlphaNum4()
		return assetCode(an.AssetCode[:]) + "-" + an.Issuer.Address()
	case xdr.AssetTypeAssetTypeCreditAlphanum12:
		an := t.MustAlphaNum12()
		return assetCode(an.AssetCode[:]) + "-" + an.Issuer.Address()
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

// changeTrustAsset renders a change-trust line: a regular asset, or a marker
// for a liquidity-pool-share trustline.
func changeTrustAsset(c xdr.ChangeTrustAsset) string {
	switch c.Type {
	case xdr.AssetTypeAssetTypeNative:
		return "native"
	case xdr.AssetTypeAssetTypeCreditAlphanum4:
		an := c.MustAlphaNum4()
		return assetCode(an.AssetCode[:]) + "-" + an.Issuer.Address()
	case xdr.AssetTypeAssetTypeCreditAlphanum12:
		an := c.MustAlphaNum12()
		return assetCode(an.AssetCode[:]) + "-" + an.Issuer.Address()
	case xdr.AssetTypeAssetTypePoolShare:
		return "liquidity_pool_share"
	default:
		return "unknown_asset"
	}
}

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

// MemoTypeName normalises the lake's XDR memo-type enum string
// ("MemoTypeMemoText") to the explorer's wire value ("text"). Returns "none"
// for the empty / no-memo case.
func MemoTypeName(xdrEnum string) string {
	s := strings.TrimPrefix(xdrEnum, "MemoTypeMemo")
	if s == "" {
		return "none"
	}
	return strings.ToLower(s)
}
