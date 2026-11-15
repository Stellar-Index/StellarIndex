package clickhouse

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

func tiedOfferXDR(t *testing.T, amount int64) string {
	t.Helper()
	seller := xdr.MustAddress("GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ")
	le := xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeOffer, Offer: &xdr.OfferEntry{
		SellerId: seller, OfferId: 7,
		Selling: xdr.MustNewNativeAsset(), Buying: xdr.MustNewCreditAsset("USDC", seller.Address()),
		Amount: xdr.Int64(amount), Price: xdr.Price{N: 1, D: 2},
	}}}
	b64, err := xdr.MarshalBase64(le)
	if err != nil {
		t.Fatal(err)
	}
	return b64
}

// The pre-image never reaches resolveTiedOffer (the query drops `state` rows); among the rest a removal
// is terminal, one transaction's last change is the post-image, and two transactions cannot be ordered.
func TestResolveTiedOffer(t *testing.T) {
	if got := resolveTiedOffer(nil); got.Removed || got.HasPost {
		t.Errorf("no change rows: %+v, want unsettled", got)
	}
	got := resolveTiedOffer([]tiedOfferChange{
		{txHash: "a", opIndex: 0, changeIndex: 5, entryXDR: tiedOfferXDR(t, 10)},
		{txHash: "a", opIndex: 0, changeIndex: 3, entryXDR: tiedOfferXDR(t, 400)},
		{txHash: "a", opIndex: 0, changeIndex: 5, entryXDR: tiedOfferXDR(t, 10)}, // a duplicated lake row
	})
	if !got.HasPost || got.Post.Amount != 10 {
		t.Errorf("one transaction: %+v, want the last change's amount 10", got)
	}
	got = resolveTiedOffer([]tiedOfferChange{
		{txHash: "a", opIndex: 0, changeIndex: 1, entryXDR: tiedOfferXDR(t, 400)},
		{txHash: "b", opIndex: 0, changeIndex: 1, entryXDR: tiedOfferXDR(t, 10)},
	})
	if got.Removed || got.HasPost {
		t.Errorf("two transactions: %+v, want unsettled", got)
	}
	got = resolveTiedOffer([]tiedOfferChange{
		{txHash: "a", opIndex: 0, changeIndex: 1, entryXDR: tiedOfferXDR(t, 400)},
		{txHash: "b", opIndex: 0, changeIndex: 1, removed: true},
	})
	if !got.Removed {
		t.Errorf("a removal among the rows: %+v, want removed", got)
	}
	if got := resolveTiedOffer([]tiedOfferChange{{txHash: "a", entryXDR: "not xdr"}}); got.Removed || got.HasPost {
		t.Errorf("undecodable post-image: %+v, want unsettled", got)
	}
}
