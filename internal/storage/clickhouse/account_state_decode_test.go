package clickhouse

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

const decodeTestAccount = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

func mustEntryB64(t *testing.T, data xdr.LedgerEntryData) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(xdr.LedgerEntry{LastModifiedLedgerSeq: 9, Data: data})
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	return b64
}

// The reserve inputs live in AccountEntry ext.v1 (liabilities) and
// ext.v1.ext.v2 (sponsorship counters); both must be decoded.
func TestAccountStateFromEntry_DecodesLiabilitiesAndSponsorship(t *testing.T) {
	acc := xdr.AccountEntry{
		AccountId:     xdr.MustAddress(decodeTestAccount),
		Balance:       50_000_000,
		SeqNum:        7,
		NumSubEntries: 3,
		Thresholds:    xdr.Thresholds{1, 0, 0, 0},
		Ext: xdr.AccountEntryExt{V: 1, V1: &xdr.AccountEntryExtensionV1{
			Liabilities: xdr.Liabilities{Buying: 11_000_000, Selling: 22_000_000},
			Ext: xdr.AccountEntryExtensionV1Ext{V: 2, V2: &xdr.AccountEntryExtensionV2{
				NumSponsored:        3,
				NumSponsoring:       1,
				SignerSponsoringIDs: []xdr.SponsorshipDescriptor{},
			}},
		}},
	}
	b64 := mustEntryB64(t, xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount, Account: &acc})

	st, ok := accountStateFromEntry(b64, 50_000_000, 9)
	if !ok || !st.Exists {
		t.Fatalf("accountStateFromEntry ok=%v exists=%v, want a live account", ok, st.Exists)
	}
	if st.BuyingLiabilities != 11_000_000 || st.SellingLiabilities != 22_000_000 {
		t.Errorf("liabilities buying=%d selling=%d, want 11000000/22000000", st.BuyingLiabilities, st.SellingLiabilities)
	}
	if st.NumSponsored != 3 || st.NumSponsoring != 1 {
		t.Errorf("sponsorship sponsored=%d sponsoring=%d, want 3/1", st.NumSponsored, st.NumSponsoring)
	}
	if st.NumSubEntries != 3 || st.SeqNum != 7 || st.LastModifiedLedger != 9 || st.MasterWeight != 1 {
		t.Errorf("base fields = %+v", st)
	}
}

// A pre-protocol-14 entry (no extensions) decodes with zero counters.
func TestAccountStateFromEntry_NoExtensionIsZero(t *testing.T) {
	acc := xdr.AccountEntry{AccountId: xdr.MustAddress(decodeTestAccount), Balance: 1, Thresholds: xdr.Thresholds{1, 0, 0, 0}}
	b64 := mustEntryB64(t, xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount, Account: &acc})
	st, ok := accountStateFromEntry(b64, 1, 9)
	if !ok || st.BuyingLiabilities != 0 || st.SellingLiabilities != 0 || st.NumSponsored != 0 || st.NumSponsoring != 0 {
		t.Errorf("st=%+v ok=%v, want a live account with zero extension fields", st, ok)
	}
	if _, ok := accountStateFromEntry("not-xdr", 1, 9); ok {
		t.Error("corrupt XDR must degrade to ok=false")
	}
}

func TestTrustlineStateFromEntry_LiabilitiesAndPoolShare(t *testing.T) {
	var code xdr.AssetCode4
	copy(code[:], "USDC")
	tl := xdr.TrustLineEntry{
		AccountId: xdr.MustAddress(decodeTestAccount),
		Asset: xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AlphaNum4: &xdr.AlphaNum4{
			AssetCode: code, Issuer: xdr.MustAddress(decodeTestAccount),
		}},
		Balance: 500,
		Limit:   1_000,
		Flags:   1,
		Ext: xdr.TrustLineEntryExt{V: 1, V1: &xdr.TrustLineEntryV1{
			Liabilities: xdr.Liabilities{Buying: 40, Selling: 250},
		}},
	}
	b64 := mustEntryB64(t, xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeTrustline, TrustLine: &tl})

	got := trustlineStateFromEntry("USDC-"+decodeTestAccount, b64, 500)
	want := TrustlineState{
		Asset: "USDC-" + decodeTestAccount, Balance: 500, Limit: 1_000, Flags: 1,
		BuyingLiabilities: 40, SellingLiabilities: 250,
	}
	if got != want {
		t.Errorf("trustline = %+v, want %+v", got, want)
	}

	pool := trustlineStateFromEntry("pool:7b2c", "", 183_920_000)
	if !pool.PoolShare || pool.Balance != 183_920_000 {
		t.Errorf("pool-share trustline = %+v, want PoolShare with its balance kept", pool)
	}
}
