package xdrjson

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// controlByteCode4 is an alphanum4 asset code carrying an embedded
// control byte (0x01) — the exact shape internal/canonical.AssetFromXDR
// (via NewClassicAsset/validateClassicAssetCode) refuses, and which
// stellar-core does not itself reject (see AssetFromXDR's doc comment
// and Q129/T099).
var controlByteCode4 = [4]byte{'A', 0x01, 'B', 0}

const issuerAccount = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestAssetCodeRejectsInvalidBytes pins the fix for Q129/T099: assetCode
// must not just trim trailing NULs, it must apply the same validity rule
// canonical.AssetFromXDR enforces, so xdrjson never mints an asset id the
// canonical layer would refuse.
func TestAssetCodeRejectsInvalidBytes(t *testing.T) {
	if code, ok := assetCode(controlByteCode4[:]); ok {
		t.Fatalf("assetCode(%v) = (%q, true), want ok=false (control byte must be rejected)", controlByteCode4, code)
	}

	if code, ok := assetCode([]byte("USDC")); !ok || code != "USDC" {
		t.Fatalf("assetCode(USDC) = (%q, %v), want (\"USDC\", true)", code, ok)
	}
}

// TestAssetIDRejectsInvalidCode proves assetID no longer emits a partial
// "A\x01B-ISSUER" id for a code AssetFromXDR would reject — it must fall
// back to the same "unknown_asset" sentinel used for other unrepresentable
// inputs, matching AssetFromXDR's contract of erroring rather than
// producing a partial id.
func TestAssetIDRejectsInvalidCode(t *testing.T) {
	issuer := xdr.MustAddress(issuerAccount)
	a := xdr.Asset{
		Type: xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{
			AssetCode: controlByteCode4,
			Issuer:    issuer,
		},
	}

	got := assetID(a)
	if got != "unknown_asset" {
		t.Fatalf("assetID(control-byte code) = %q, want \"unknown_asset\"", got)
	}
}

// TestTrustLineAssetIDRejectsInvalidCode is the TrustLineAssetID sibling
// of TestAssetIDRejectsInvalidCode — same defect, same fix, same guard.
func TestTrustLineAssetIDRejectsInvalidCode(t *testing.T) {
	issuer := xdr.MustAddress(issuerAccount)
	tl := xdr.TrustLineAsset{
		Type: xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{
			AssetCode: controlByteCode4,
			Issuer:    issuer,
		},
	}

	got := TrustLineAssetID(tl)
	if got != "unknown_asset" {
		t.Fatalf("TrustLineAssetID(control-byte code) = %q, want \"unknown_asset\"", got)
	}
}
