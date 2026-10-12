package xdrjson

import (
	"encoding/hex"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// controlByteCode4 is an alphanum4 asset code with an embedded control byte (0x01): the shape canonical.AssetFromXDR refuses
// (NewClassicAsset/validateClassicAssetCode) but stellar-core does not reject.
var controlByteCode4 = [4]byte{'A', 0x01, 'B', 0}

const issuerAccount = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestAssetCodeRejectsInvalidBytes pins that assetCode applies the same validity rule as canonical.AssetFromXDR, not just
// trimming trailing NULs, so xdrjson never mints an asset id the canonical layer would refuse.
func TestAssetCodeRejectsInvalidBytes(t *testing.T) {
	if code, ok := assetCode(controlByteCode4[:]); ok {
		t.Fatalf("assetCode(%v) = (%q, true), want ok=false (control byte must be rejected)", controlByteCode4, code)
	}

	if code, ok := assetCode([]byte("USDC")); !ok || code != "USDC" {
		t.Fatalf("assetCode(USDC) = (%q, %v), want (\"USDC\", true)", code, ok)
	}
}

// TestAssetIDRejectsInvalidCode: assetID must fall back to the "unknown_asset" sentinel for a
// code AssetFromXDR would reject, never emit a partial "A\x01B-ISSUER" id.
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

// TestChangeTrustAssetPoolShare: a change_trust op on a liquidity-pool share must render the
// SAME "pool:<hex>" id the resulting trustline gets from TrustLineAssetID. The hex is
// re-derived via xdr.NewPoolId (the network's own pool-id hash) to prove a byte-exact identity.
func TestChangeTrustAssetPoolShare(t *testing.T) {
	issuer := xdr.MustAddress(issuerAccount)
	assetB := xdr.Asset{
		Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{AssetCode: [4]byte{'U', 'S', 'D', 'C'}, Issuer: issuer},
	}
	assetA := xdr.MustNewNativeAsset()
	const fee = xdr.Int32(30)

	wantID, err := xdr.NewPoolId(assetA, assetB, fee)
	if err != nil {
		t.Fatalf("xdr.NewPoolId: %v", err)
	}

	line := xdr.ChangeTrustAsset{
		Type: xdr.AssetTypeAssetTypePoolShare,
		LiquidityPool: &xdr.LiquidityPoolParameters{
			Type: xdr.LiquidityPoolTypeLiquidityPoolConstantProduct,
			ConstantProduct: &xdr.LiquidityPoolConstantProductParameters{
				AssetA: assetA,
				AssetB: assetB,
				Fee:    fee,
			},
		},
	}

	got := changeTrustAsset(line)
	want := "pool:" + hex.EncodeToString(wantID[:])
	if got != want {
		t.Fatalf("changeTrustAsset(pool share) = %q, want %q", got, want)
	}
	if got == "liquidity_pool_share" {
		t.Fatalf("changeTrustAsset still returns the old opaque marker")
	}

	// Matches TrustLineAssetID's spelling for the SAME pool — same concept,
	// one spelling.
	tl := xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypePoolShare, LiquidityPoolId: &wantID}
	if tlGot := TrustLineAssetID(tl); tlGot != got {
		t.Fatalf("change_trust line %q disagrees with the trustline's own id %q for the same pool", got, tlGot)
	}
}

func TestLedgerEntryFields_AccountCarriesSignerAndEntrySponsors(t *testing.T) {
	const (
		owner   = "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7"
		signer  = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"
		sponsor = "GCEZWKCA5VLDNRLN3RPRJMRZOX3Z6G5CHCGSNFHEYVXM3XOJMDS674JZ"
	)
	e := xdr.LedgerEntry{
		LastModifiedLedgerSeq: 99,
		Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount, Account: &xdr.AccountEntry{
			AccountId:  xdr.MustAddress(owner),
			Balance:    1 << 62,
			SeqNum:     12,
			Thresholds: xdr.Thresholds{1, 2, 3, 4},
			HomeDomain: "example.com",
			Signers:    []xdr.Signer{{Key: xdr.MustSigner(signer), Weight: 5}},
			Ext: xdr.AccountEntryExt{V: 1, V1: &xdr.AccountEntryExtensionV1{Ext: xdr.AccountEntryExtensionV1Ext{
				V: 2, V2: &xdr.AccountEntryExtensionV2{SignerSponsoringIDs: []xdr.SponsorshipDescriptor{xdr.MustAddressPtr(sponsor)}},
			}}},
		}},
		Ext: xdr.LedgerEntryExt{V: 1, V1: &xdr.LedgerEntryExtensionV1{SponsoringId: xdr.MustAddressPtr(sponsor)}},
	}
	f, ok := LedgerEntryFields(e)
	if !ok {
		t.Fatal("account entry not decoded")
	}
	if f["balance"] != "4611686018427387904" || f["seq_num"] != "12" || f["home_domain"] != "example.com" {
		t.Fatalf("balance/seq_num/home_domain = %v/%v/%v", f["balance"], f["seq_num"], f["home_domain"])
	}
	if f["master_weight"] != uint8(1) || f["threshold_high"] != uint8(4) {
		t.Fatalf("master_weight/threshold_high = %v/%v, want 1/4", f["master_weight"], f["threshold_high"])
	}
	if f["sponsor"] != sponsor {
		t.Fatalf("sponsor = %v, want %s", f["sponsor"], sponsor)
	}
	signers, _ := f["signers"].([]map[string]any)
	if len(signers) != 1 || signers[0]["key"] != signer || signers[0]["weight"] != uint32(5) || signers[0]["sponsor"] != sponsor {
		t.Fatalf("signers = %v, want one %s weight 5 sponsored by %s", f["signers"], signer, sponsor)
	}
	if _, present := f["last_modified_ledger"]; present {
		t.Fatal("last_modified_ledger must be left out: it would mark every diff as changed")
	}
}

func TestLedgerEntryFields_RejectsNonClassicEntries(t *testing.T) {
	if _, ok := LedgerEntryFields(xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeContractCode}}); ok {
		t.Fatal("contract code entry decoded, want ok=false")
	}
}

const pubnet = "Public Global Stellar Network ; September 2015"

func TestSACContractID(t *testing.T) {
	cases := []struct {
		asset string
		want  string
	}{
		// Native XLM's SAC on pubnet (well-known).
		{"native", "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"},
		// USDC (Circle issuer) SAC — appears as a Blend reserve.
		{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"},
	}
	for _, c := range cases {
		got, ok := SACContractID(c.asset, pubnet)
		if !ok {
			t.Errorf("SACContractID(%q) ok=false", c.asset)
			continue
		}
		if got != c.want {
			t.Errorf("SACContractID(%q) = %s, want %s", c.asset, got, c.want)
		}
	}
	// Non-SAC assets → ok=false.
	for _, a := range []string{"fiat:USD", "crypto:BTC", "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"} {
		if _, ok := SACContractID(a, pubnet); ok {
			t.Errorf("SACContractID(%q) ok=true, want false", a)
		}
	}
}
