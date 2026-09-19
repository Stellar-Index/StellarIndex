package clickhouse

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

const (
	offerTestSeller = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	offerTestIssuer = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
)

// offerEntryB64 builds REAL LedgerEntry bytes (SDK-marshalled XDR) for
// an offer selling native for USDC at price n/d.
func offerEntryB64(t *testing.T, offerID int64, amount int64, n, d int32) string {
	t.Helper()
	entry := xdr.LedgerEntry{
		LastModifiedLedgerSeq: 63_000_000,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeOffer,
			Offer: &xdr.OfferEntry{
				SellerId: xdr.MustAddress(offerTestSeller),
				OfferId:  xdr.Int64(offerID),
				Selling:  xdr.MustNewNativeAsset(),
				Buying:   xdr.MustNewCreditAsset("USDC", offerTestIssuer),
				Amount:   xdr.Int64(amount),
				Price:    xdr.Price{N: xdr.Int32(n), D: xdr.Int32(d)},
			},
		},
	}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal offer entry: %v", err)
	}
	return b64
}

func TestOfferFromEntryXDR_DecodesRealBytes(t *testing.T) {
	o, ok := offerFromEntryXDR(offerEntryB64(t, 42, 1_000_000_000, 3, 2))
	if !ok {
		t.Fatal("decode failed on a valid offer entry")
	}
	if o.OfferID != 42 || o.Amount != 1_000_000_000 {
		t.Errorf("offer id/amount = %d/%d, want 42/1000000000", o.OfferID, o.Amount)
	}
	if o.Seller != offerTestSeller {
		t.Errorf("seller = %q, want %q", o.Seller, offerTestSeller)
	}
	// Canonical asset rendering — MUST match the wire forms the rest of
	// the API uses (`native`, `CODE-G...`), or the handler's pair match
	// silently never fires.
	if o.Selling != "native" {
		t.Errorf("selling = %q, want native", o.Selling)
	}
	if want := "USDC-" + offerTestIssuer; o.Buying != want {
		t.Errorf("buying = %q, want %q", o.Buying, want)
	}
	if o.PriceN != 3 || o.PriceD != 2 {
		t.Errorf("price = %d/%d, want 3/2", o.PriceN, o.PriceD)
	}
}

func TestOfferFromEntryXDR_RefusesNonOfferAndGarbage(t *testing.T) {
	// A trustline entry — valid XDR, wrong entry type.
	tl := xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeTrustline,
			TrustLine: &xdr.TrustLineEntry{
				AccountId: xdr.MustAddress(offerTestSeller),
				Asset:     xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypeNative},
			},
		},
	}
	b64, err := xdr.MarshalBase64(tl)
	if err != nil {
		t.Fatalf("marshal trustline: %v", err)
	}
	if _, ok := offerFromEntryXDR(b64); ok {
		t.Error("non-offer entry must not decode as an offer")
	}
	if _, ok := offerFromEntryXDR("not base64 xdr"); ok {
		t.Error("garbage must not decode")
	}
	if _, ok := offerFromEntryXDR(""); ok {
		t.Error("empty must not decode")
	}
}

func TestOfferVersion_MatchesTableDefinition(t *testing.T) {
	// version = (ledger_seq << 32) | intra_ledger_seq — must mirror the
	// ledger_entries_current MATERIALIZED column exactly.
	if got := offerVersion(2, 3); got != (uint64(2)<<32)|3 {
		t.Errorf("offerVersion(2,3) = %d", got)
	}
	if offerVersion(63_000_000, 0) <= offerVersion(62_999_999, 4_000_000_000) {
		t.Error("a later ledger must always order above any intra seq of an earlier ledger")
	}
}

// TestOfferBookTip pins the order book's hole-safe read bound (F162): the
// cursor holds just below a missing ledger, resumes through it once it is
// filled, and a read with no cursor to continue from starts at the lake's
// first ledger instead of reporting a boundary hole forever.
func TestOfferBookTip(t *testing.T) {
	const cursor = uint32(63_400_000)
	cases := []struct {
		name     string
		from     uint32
		anchored bool
		lc       ledgerContiguity
		want     uint32
	}{
		{
			name: "contiguous lake: read to the tip",
			from: cursor + 1, anchored: true,
			lc:   ledgerContiguity{lakeMax: cursor + 12, minPresent: cursor + 1},
			want: cursor + 12,
		},
		{
			name: "hole directly above the cursor: hold at the cursor",
			from: cursor + 1, anchored: true,
			lc:   ledgerContiguity{lakeMax: cursor + 12, minPresent: cursor + 2},
			want: cursor,
		},
		{
			name: "hole further up: advance to just below it, not past it",
			from: cursor + 1, anchored: true,
			lc:   ledgerContiguity{lakeMax: cursor + 12, firstGap: cursor + 7, minPresent: cursor + 1},
			want: cursor + 6,
		},
		{
			name: "lake has not passed the cursor: idle",
			from: cursor + 1, anchored: true,
			lc:   ledgerContiguity{lakeMax: cursor},
			want: cursor,
		},
		{
			name: "book loaded off an empty lake, lake now begins at ledger 2: start there",
			from: 1, anchored: false,
			lc:   ledgerContiguity{lakeMax: 40, minPresent: 2},
			want: 40,
		},
		{
			name: "the same read anchored would stall forever on the never-exported ledger 1",
			from: 1, anchored: true,
			lc:   ledgerContiguity{lakeMax: 40, minPresent: 2},
			want: 0,
		},
		{
			name: "full load, open hole inside the lookback window: cursor starts below it",
			from: cursor - 100_000, anchored: false,
			lc:   ledgerContiguity{lakeMax: cursor, firstGap: cursor - 40, minPresent: cursor - 100_000},
			want: cursor - 41,
		},
		{
			name: "full load on a lake shorter than the lookback: anchor at its first ledger",
			from: 0, anchored: false,
			lc:   ledgerContiguity{lakeMax: 900, minPresent: 2},
			want: 900,
		},
		{
			name: "empty lake",
			from: 1, anchored: false,
			lc:   ledgerContiguity{},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := offerBookTip(tc.from, tc.anchored, tc.lc); got != tc.want {
				t.Errorf("offerBookTip(from=%d, anchored=%v, %+v) = %d, want %d", tc.from, tc.anchored, tc.lc, got, tc.want)
			}
		})
	}
}
