package explorer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// assetEntryChangesReader serves the account fake's rows through the asset index and records what was asked.
type assetEntryChangesReader struct {
	*accountEntryChangesReader
	gotAsset    string
	gotTypes    []string
	ledgerCalls int
}

func newAssetEntryChangesReader(wm uint32, rows []clickhouse.EntryHistorySourceRow) *assetEntryChangesReader {
	return &assetEntryChangesReader{accountEntryChangesReader: newAccountEntryChangesReader(wm, rows)}
}

func (r *assetEntryChangesReader) AssetEntryLedgers(_ context.Context, asset string, entryTypes []string, maxLedger uint32, limit int) ([]uint32, error) {
	r.gotAsset, r.gotTypes = asset, entryTypes
	r.ledgerCalls++
	var out []uint32
	for _, l := range r.ledgers {
		if l <= maxLedger && len(out) < limit {
			out = append(out, l)
		}
	}
	return out, nil
}

func aecClaimable(amount int64) xdr.LedgerEntry {
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeClaimableBalance, ClaimableBalance: &xdr.ClaimableBalanceEntry{
		BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &xdr.Hash{7}},
		Claimants: []xdr.Claimant{{Type: xdr.ClaimantTypeClaimantTypeV0, V0: &xdr.ClaimantV0{
			Destination: xdr.MustAddress(aecOther), Predicate: xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional},
		}}},
		Asset: xdr.MustNewCreditAsset("USD", aecIssuer), Amount: xdr.Int64(amount),
	}}}
}

func aecOffer(seller string, amount int64) xdr.LedgerEntry {
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeOffer, Offer: &xdr.OfferEntry{
		SellerId: xdr.MustAddress(seller), OfferId: 1, Selling: xdr.MustNewCreditAsset("USD", aecIssuer),
		Buying: xdr.MustNewNativeAsset(), Amount: xdr.Int64(amount), Price: xdr.Price{N: 1, D: 1},
	}}}
}

func aecTypedRow(t *testing.T, entryType string, ledger, seq uint32, tx string, change uint32, changeType string, e xdr.LedgerEntry) clickhouse.EntryHistorySourceRow {
	t.Helper()
	s := aecRow(t, ledger, seq, tx, change, changeType, e)
	s.EntryType = entryType
	return s
}

// aecAssetFixture adds to aecFixture a EUR trustline and a USD offer in ledger 100 (neither is USD history) and a
// USD claimable balance sponsored in ledger 95.
func aecAssetFixture(t *testing.T) []clickhouse.EntryHistorySourceRow {
	eur := xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeTrustline, TrustLine: &xdr.TrustLineEntry{
		AccountId: xdr.MustAddress(aecOther), Asset: xdr.MustNewCreditAsset("EUR", aecIssuer).ToTrustLineAsset(),
		Balance: 7, Limit: 1 << 62,
	}}}
	return append(aecFixture(t),
		aecRow(t, 100, 6, "e1", 0, "created", eur),
		aecTypedRow(t, "offer", 100, 5, "o1", 0, "created", aecOffer(aecOther, 3)),
		aecTypedRow(t, "claimable_balance", 95, 1, "cb", 0, "created", aecSponsored(aecClaimable(70), aecSponsor)),
	)
}

func callAssetEntryChanges(t *testing.T, reader *assetEntryChangesReader, assetID string, q url.Values, limit int) (int, AssetEntryChangesView) {
	t.Helper()
	return serveAssetEntryChanges(t, newProbeHandler(reader, nil), assetID, q, limit)
}

func serveAssetEntryChanges(t *testing.T, h *Handler, assetID string, q url.Values, limit int) (int, AssetEntryChangesView) {
	t.Helper()
	var view AssetEntryChangesView
	h.ParseLimit = func(http.ResponseWriter, *http.Request, int, int) (int, bool) { return limit, true }
	h.WriteJSONAt = func(w http.ResponseWriter, v any, _, _ bool, _ time.Time) {
		view = v.(AssetEntryChangesView)
		w.WriteHeader(http.StatusOK)
	}
	h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) {
		view = v.(AssetEntryChangesView)
		w.WriteHeader(http.StatusOK)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/assets/x/entry-changes?"+q.Encode(), nil)
	r.SetPathValue("asset_id", assetID)
	rec := httptest.NewRecorder()
	h.AssetEntryChanges(rec, r)
	return rec.Code, view
}

func TestAssetEntryChanges_NewestFirstForTheAssetOnly(t *testing.T) {
	reader := newAssetEntryChangesReader(120, aecAssetFixture(t))
	code, view := callAssetEntryChanges(t, reader, aecUSD, nil, 25)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if reader.gotAsset != aecUSD || !slices.Equal(reader.gotTypes, assetEntryTypes) {
		t.Errorf("index asked for %q %v, want %q %v", reader.gotAsset, reader.gotTypes, aecUSD, assetEntryTypes)
	}
	want := []struct {
		ledger                    uint32
		tx, role, account, amount string
	}{
		{100, "a1", "holder", validTestAccount, "40"},
		{100, "b1", "holder", validTestAccount, "25"},
		{100, "99", "holder", aecOther, "5"},
		{95, "cb", "claimable", aecSponsor, "70"},
		{90, "c1", "holder", validTestAccount, "10"},
	}
	if len(view.Changes) != len(want) {
		t.Fatalf("%d changes, want %d: %+v", len(view.Changes), len(want), view.Changes)
	}
	for i, w := range want {
		c := view.Changes[i]
		if c.Ledger != w.ledger || c.TxHash != w.tx || c.Role != w.role || c.Account != w.account || c.Amount != w.amount {
			t.Errorf("change %d = %d/%s/%s/%s/%s, want %d/%s/%s/%s/%s", i, c.Ledger, c.TxHash, c.Role, c.Account, c.Amount,
				w.ledger, w.tx, w.role, w.account, w.amount)
		}
	}
	if view.Changes[0].IntraLedgerSeq != 4 || view.Changes[3].EntryType != "claimable_balance" {
		t.Errorf("seq %d / type %q, want 4 / claimable_balance", view.Changes[0].IntraLedgerSeq, view.Changes[3].EntryType)
	}
	if view.Asset != aecUSD || view.NextCursor != "" || !view.LowerBound || view.ThroughLedger != 120 {
		t.Errorf("asset %q cursor %q lower_bound %v through %d", view.Asset, view.NextCursor, view.LowerBound, view.ThroughLedger)
	}
	if !strings.Contains(view.CoverageNote, "offers are not served") || !strings.Contains(view.CoverageNote, "trustline changes only") {
		t.Errorf("coverage note does not name the excluded offers and the trustline-only index: %q", view.CoverageNote)
	}
}

func TestAssetEntryChanges_CursorWalksInsideALedger(t *testing.T) {
	reader := newAssetEntryChangesReader(120, aecAssetFixture(t))
	q := url.Values{}
	var got []string
	for range 8 {
		code, view := callAssetEntryChanges(t, reader, aecUSD, q, 1)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		for _, c := range view.Changes {
			got = append(got, fmt.Sprintf("%d/%s/%s", c.Ledger, c.TxHash, c.Amount))
		}
		if view.NextCursor == "" {
			break
		}
		q.Set("cursor", view.NextCursor)
	}
	want := []string{"100/a1/40", "100/b1/25", "100/99/5", "95/cb/70", "90/c1/10"}
	if !slices.Equal(got, want) {
		t.Errorf("walk = %v, want %v", got, want)
	}
}

func TestAssetEntryChanges_FirstPageCachedAndCutPerLimit(t *testing.T) {
	reader := newAssetEntryChangesReader(120, aecAssetFixture(t))
	h := newProbeHandler(reader, nil)
	code, full := serveAssetEntryChanges(t, h, aecUSD, nil, 25)
	if code != http.StatusOK || len(full.Changes) != 5 {
		t.Fatalf("status %d changes %d; want 200 and 5", code, len(full.Changes))
	}
	calls := reader.ledgerCalls
	_, cut := serveAssetEntryChanges(t, h, aecUSD, nil, 2)
	if reader.ledgerCalls != calls {
		t.Errorf("second first-page request walked the index again (%d calls, want %d)", reader.ledgerCalls, calls)
	}
	_, uncached := callAssetEntryChanges(t, newAssetEntryChangesReader(120, aecAssetFixture(t)), aecUSD, url.Values{"cursor": {cut.NextCursor}}, 25)
	var got []string
	for _, c := range append(cut.Changes, uncached.Changes...) {
		got = append(got, c.TxHash)
	}
	var want []string
	for _, c := range full.Changes {
		want = append(want, c.TxHash)
	}
	if len(cut.Changes) != 2 || !slices.Equal(got, want) {
		t.Errorf("cut page %d rows then cursor walk = %v, want 2 rows then %v", len(cut.Changes), got, want)
	}
}

func TestAssetEntryChanges_AliasResolvesToNative(t *testing.T) {
	reader := newAssetEntryChangesReader(120, aecAssetFixture(t))
	code, view := callAssetEntryChanges(t, reader, "crypto:XLM", nil, 25)
	if code != http.StatusOK || reader.gotAsset != "native" || len(view.Changes) != 0 || view.ThroughLedger != 120 {
		t.Errorf("status %d asset %q changes %d through %d; want 200 native 0 120", code, reader.gotAsset, len(view.Changes), view.ThroughLedger)
	}
}

func TestAssetEntryChanges_NotBuiltIs503(t *testing.T) {
	reader := newAssetEntryChangesReader(0, aecAssetFixture(t))
	if code, _ := callAssetEntryChanges(t, reader, aecUSD, nil, 25); code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 while the entry index is not built", code)
	}
}

func TestAssetEntryChanges_RejectsBadInput(t *testing.T) {
	reader := newAssetEntryChangesReader(120, aecAssetFixture(t))
	for _, cur := range []string{"x", "1.a.b.c.d", "0.t.0.0.r", "5.t.-2.0.r", "5.t.0.0."} {
		if code, _ := callAssetEntryChanges(t, reader, aecUSD, url.Values{"cursor": {cur}}, 25); code != http.StatusBadRequest {
			t.Errorf("cursor %q: status = %d, want 400", cur, code)
		}
	}
	if code, _ := callAssetEntryChanges(t, reader, "fiat:USD", nil, 25); code != http.StatusBadRequest {
		t.Errorf("fiat asset: status = %d, want 400", code)
	}
}
