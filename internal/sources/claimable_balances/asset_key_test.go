package claimable_balances

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// TestAssetKeyFromAsset_nonEd25519IssuerNoPanic pins that the issuer deref uses GetEd25519() with the ok-check, not the raw
// slice, which panics on a non-Ed25519 AccountId; the decoder must return an error.
func TestAssetKeyFromAsset_nonEd25519IssuerNoPanic(t *testing.T) {
	// AccountId with a type that has no Ed25519 union arm populated.
	badIssuer := xdr.AccountId{Type: xdr.PublicKeyType(99)} // Ed25519 == nil

	cases := []struct {
		name  string
		asset xdr.Asset
	}{
		{
			name: "alphanum4",
			asset: xdr.Asset{
				Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
				AlphaNum4: &xdr.AlphaNum4{AssetCode: [4]byte{'U', 'S', 'D', 'C'}, Issuer: badIssuer},
			},
		},
		{
			name: "alphanum12",
			asset: xdr.Asset{
				Type:       xdr.AssetTypeAssetTypeCreditAlphanum12,
				AlphaNum12: &xdr.AlphaNum12{AssetCode: [12]byte{'L', 'O', 'N', 'G'}, Issuer: badIssuer},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := assetKeyFromAsset(tc.asset)
			if err == nil {
				t.Fatal("assetKeyFromAsset: want error on non-Ed25519 issuer, got nil")
			}
		})
	}
}
