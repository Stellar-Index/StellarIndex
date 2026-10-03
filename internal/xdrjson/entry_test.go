package xdrjson

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

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
