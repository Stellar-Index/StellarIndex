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

// A transaction's `state` row is the pre-image and never served; a removal is terminal; the post-image is the
// last change in apply order, across transactions only when their positions are known; a snapshot row with
// no transaction leaves the loaded row standing.
func TestResolveTiedOffer(t *testing.T) {
	settled := func(r TiedOffer) bool { return r.Removed || r.HasPost || r.Loaded }
	if got := resolveTiedOffer(nil, nil); settled(got) {
		t.Errorf("no change rows: %+v, want unsettled", got)
	}
	got := resolveTiedOffer([]tiedOfferChange{
		{txHash: "a", changeIndex: 4, changeType: "state", entryXDR: tiedOfferXDR(t, 1000)},
		{txHash: "a", changeIndex: 5, changeType: "updated", entryXDR: tiedOfferXDR(t, 10)},
		{txHash: "a", changeIndex: 3, changeType: "updated", entryXDR: tiedOfferXDR(t, 400)},
		{txHash: "a", changeIndex: 5, changeType: "updated", entryXDR: tiedOfferXDR(t, 10)}, // a duplicated lake row
	}, nil)
	if !got.HasPost || got.Post.Amount != 10 {
		t.Errorf("one transaction: %+v, want the last change's amount 10", got)
	}
	two := []tiedOfferChange{
		{txHash: "a", changeIndex: 9, changeType: "updated", entryXDR: tiedOfferXDR(t, 400)},
		{txHash: "b", changeIndex: 1, changeType: "updated", entryXDR: tiedOfferXDR(t, 10)},
	}
	if got := resolveTiedOffer(two, map[string]uint32{"a": 3}); settled(got) {
		t.Errorf("two transactions, one position unknown: %+v, want unsettled", got)
	}
	if got := resolveTiedOffer(two, map[string]uint32{"a": 3, "b": 7}); !got.HasPost || got.Post.Amount != 10 {
		t.Errorf("two transactions, b applied later: %+v, want amount 10", got)
	}
	if got := resolveTiedOffer(two, map[string]uint32{"a": 7, "b": 3}); !got.HasPost || got.Post.Amount != 400 {
		t.Errorf("two transactions, a applied later: %+v, want amount 400", got)
	}
	// A removal settles it wherever it sits among the rows.
	for _, rows := range [][]tiedOfferChange{
		{{txHash: "a", changeType: "updated", entryXDR: tiedOfferXDR(t, 400)}, {txHash: "b", changeType: "updated", entryXDR: tiedOfferXDR(t, 9)}, {txHash: "b", changeType: "removed"}},
		{{txHash: "b", changeType: "removed"}, {txHash: "a", changeType: "updated", entryXDR: tiedOfferXDR(t, 400)}},
	} {
		if got := resolveTiedOffer(rows, nil); !got.Removed {
			t.Errorf("a removal among the rows: %+v, want removed", got)
		}
	}
	if got := resolveTiedOffer([]tiedOfferChange{{changeType: "state", entryXDR: tiedOfferXDR(t, 70)}}, nil); !got.Loaded {
		t.Errorf("only a snapshot row: %+v, want the loaded row to stand", got)
	}
	if got := resolveTiedOffer([]tiedOfferChange{{txHash: "a", changeType: "state", entryXDR: tiedOfferXDR(t, 70)}}, nil); settled(got) {
		t.Errorf("only a transaction's pre-image: %+v, want unsettled", got)
	}
	if got := resolveTiedOffer([]tiedOfferChange{{txHash: "a", changeType: "updated", entryXDR: "not xdr"}}, nil); settled(got) {
		t.Errorf("undecodable post-image: %+v, want unsettled", got)
	}
}
