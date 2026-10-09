package explorer

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type entryChangesReader struct {
	*capReader
	wm, backfilledThru uint32
	rows               []clickhouse.AssetEntryChange

	called     bool
	gotAsset   string
	gotCeiling uint32
	gotCursor  clickhouse.AssetEntryChangeCursor
}

func (r *entryChangesReader) EntryHistoryCoverage(context.Context) (uint32, uint32, error) {
	return r.wm, r.backfilledThru, nil
}

func (r *entryChangesReader) AssetEntryChanges(_ context.Context, asset string, limit int, cur clickhouse.AssetEntryChangeCursor, maxLedger uint32) ([]clickhouse.AssetEntryChange, error) {
	r.called, r.gotAsset, r.gotCeiling, r.gotCursor = true, asset, maxLedger, cur
	if len(r.rows) > limit {
		return r.rows[:limit], nil
	}
	return r.rows, nil
}

func entryChangeRow(ledger uint32, role string, balance int64) clickhouse.AssetEntryChange {
	return clickhouse.AssetEntryChange{
		EntryChangeRef: clickhouse.EntryChangeRef{
			Ledger: ledger, CloseTime: time.Unix(1_700_000_000, 0), TxHash: "ab", OpIndex: -1,
			EntryType: "trustline", ChangeType: "updated", Fields: `{"balance":"1"}`,
		},
		Role: role, Account: validTestAccount, Balance: big.NewInt(balance),
	}
}

func callAssetEntryChanges(t *testing.T, reader *entryChangesReader, assetID string, q url.Values, limit int) (int, AssetEntryChangesView) {
	t.Helper()
	var view AssetEntryChangesView
	h := newProbeHandler(reader, nil)
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

// An amount past 2^63 must reach the wire digit-for-digit; the buying side of
// an offer holds none of the asset, so it carries no amount at all.
func TestAssetEntryChanges_AmountsAreExactStrings(t *testing.T) {
	big128, _ := new(big.Int).SetString("170141183460469231731687303715884105727", 10)
	holder := entryChangeRow(90, "holder", 0)
	holder.Balance = big128
	reader := &entryChangesReader{
		capReader: &capReader{probe: &deadlineProbe{}}, wm: 100, backfilledThru: 100,
		rows: []clickhouse.AssetEntryChange{holder, entryChangeRow(80, "buying", 0)},
	}
	code, view := callAssetEntryChanges(t, reader, "USDC-"+testUSDCIssuer, nil, 25)
	if code != http.StatusOK || len(view.Changes) != 2 {
		t.Fatalf("code=%d changes=%d", code, len(view.Changes))
	}
	if got := view.Changes[0].Amount; got != big128.String() {
		t.Errorf("holder amount = %q, want %s", got, big128)
	}
	if got := view.Changes[1].Amount; got != "" {
		t.Errorf("buying amount = %q, want omitted", got)
	}
	if view.Changes[0].OpIndex != -1 || string(view.Changes[0].Entry) != `{"balance":"1"}` {
		t.Errorf("entry = %+v", view.Changes[0])
	}
	if view.LowerBound || view.NextCursor != "" {
		t.Errorf("lower_bound=%v next_cursor=%q, want a complete final page", view.LowerBound, view.NextCursor)
	}
}

// The ceiling is the derive watermark, XLM aliases fold to "native", and an
// unverified derive is a lower bound.
func TestAssetEntryChanges_CeilingAliasAndLowerBound(t *testing.T) {
	reader := &entryChangesReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: 500}
	code, view := callAssetEntryChanges(t, reader, "crypto:XLM", nil, 25)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if reader.gotAsset != "native" || reader.gotCeiling != 500 || view.ThroughLedger != 500 {
		t.Errorf("asset=%q ceiling=%d through=%d", reader.gotAsset, reader.gotCeiling, view.ThroughLedger)
	}
	if !view.LowerBound || view.CoverageNote == "" {
		t.Errorf("lower_bound=%v note=%q, want a named lower bound", view.LowerBound, view.CoverageNote)
	}
}

// With no derive on the deployment there is nothing to read below a ceiling of 0.
func TestAssetEntryChanges_NoWatermarkSkipsRead(t *testing.T) {
	reader := &entryChangesReader{capReader: &capReader{probe: &deadlineProbe{}}}
	code, view := callAssetEntryChanges(t, reader, "native", nil, 25)
	if code != http.StatusOK || reader.called {
		t.Fatalf("code=%d called=%v", code, reader.called)
	}
	if !view.LowerBound || view.Changes == nil || len(view.Changes) != 0 {
		t.Errorf("view = %+v, want an empty lower-bound page", view)
	}
}

// next_cursor round-trips, including a tx-level op_index of -1.
func TestAssetEntryChanges_CursorRoundTrip(t *testing.T) {
	row := entryChangeRow(90, "claimable", 5)
	row.ChangeIndex = 3
	reader := &entryChangesReader{
		capReader: &capReader{probe: &deadlineProbe{}}, wm: 100, backfilledThru: 1,
		rows: []clickhouse.AssetEntryChange{row},
	}
	_, view := callAssetEntryChanges(t, reader, "native", nil, 1)
	if view.NextCursor != "90.ab.-1.3.claimable" {
		t.Fatalf("next_cursor = %q", view.NextCursor)
	}
	code, _ := callAssetEntryChanges(t, reader, "native", url.Values{"cursor": {view.NextCursor}}, 1)
	want := clickhouse.AssetEntryChangeCursor{Ledger: 90, TxHash: "ab", OpIndex: -1, ChangeIndex: 3, Role: "claimable"}
	if code != http.StatusOK || reader.gotCursor != want {
		t.Errorf("code=%d cursor=%+v, want %+v", code, reader.gotCursor, want)
	}
}

func TestAssetEntryChanges_RejectsBadInput(t *testing.T) {
	reader := &entryChangesReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: 100}
	for _, c := range []string{"90.ab.-1.3", "0.ab.-1.3.holder", "90.ab.-2.3.holder", "90.ab.x.3.holder", "90.ab.1.3."} {
		if code, _ := callAssetEntryChanges(t, reader, "native", url.Values{"cursor": {c}}, 25); code != http.StatusBadRequest {
			t.Errorf("cursor %q: code=%d, want 400", c, code)
		}
	}
	if code, _ := callAssetEntryChanges(t, reader, "fiat:USD", nil, 25); code != http.StatusBadRequest {
		t.Errorf("off-chain asset: code=%d, want 400", code)
	}
	if reader.called {
		t.Error("a rejected request reached the store")
	}
}
