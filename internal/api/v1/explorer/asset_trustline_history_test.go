package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type trustlineHistoryReader struct {
	*capReader
	wm       uint32
	days     []clickhouse.AssetTrustlineDay
	gotAsset string
	gotMax   uint32
}

func (r *trustlineHistoryReader) EntryIndexWatermark(context.Context) (uint32, error) {
	return r.wm, nil
}

func (r *trustlineHistoryReader) AssetTrustlineDaily(_ context.Context, asset string, maxLedger uint32) ([]clickhouse.AssetTrustlineDay, error) {
	r.gotAsset, r.gotMax = asset, maxLedger
	return r.days, nil
}

func callTrustlineHistory(t *testing.T, reader *trustlineHistoryReader, assetID string) (int, AssetTrustlineHistoryView) {
	t.Helper()
	var view AssetTrustlineHistoryView
	h := newProbeHandler(reader, nil)
	capture := func(w http.ResponseWriter, v any) {
		view = v.(AssetTrustlineHistoryView)
		w.WriteHeader(http.StatusOK)
	}
	h.WriteJSONAt = func(w http.ResponseWriter, v any, _, _ bool, _ time.Time) { capture(w, v) }
	h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) { capture(w, v) }
	r := httptest.NewRequest(http.MethodGet, "/v1/assets/x/trustline-history", nil)
	r.SetPathValue("asset_id", assetID)
	rec := httptest.NewRecorder()
	h.AssetTrustlineHistory(rec, r)
	return rec.Code, view
}

func TestAssetTrustlineHistory_LiveIsTheRunningNet(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 4, d, 0, 0, 0, 0, time.UTC) }
	reader := &trustlineHistoryReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: 900, days: []clickhouse.AssetTrustlineDay{
		{Day: day(1), Created: 5}, {Day: day(3), Created: 2, Removed: 4}, {Day: day(9), Removed: 3},
	}}
	code, view := callTrustlineHistory(t, reader, aecUSD)
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	if reader.gotAsset != aecUSD || reader.gotMax != 900 || view.ThroughLedger != 900 {
		t.Fatalf("asked (%q, %d), through %d; want (%q, 900), 900", reader.gotAsset, reader.gotMax, view.ThroughLedger, aecUSD)
	}
	want := []AssetTrustlineDayEntry{
		{Date: "2026-04-01", Created: 5, Live: 5},
		{Date: "2026-04-03", Created: 2, Removed: 4, Live: 3},
		{Date: "2026-04-09", Removed: 3, Live: 0},
	}
	if len(view.Days) != len(want) {
		t.Fatalf("days = %+v, want %+v", view.Days, want)
	}
	for i := range want {
		if view.Days[i] != want[i] {
			t.Errorf("day %d = %+v, want %+v", i, view.Days[i], want[i])
		}
	}
}

func TestAssetTrustlineHistory_NotBuiltIs503(t *testing.T) {
	reader := &trustlineHistoryReader{capReader: &capReader{probe: &deadlineProbe{}}}
	if code, _ := callTrustlineHistory(t, reader, aecUSD); code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 before the index is built", code)
	}
}

func TestAssetTrustlineHistory_RejectsOffChainAsset(t *testing.T) {
	reader := &trustlineHistoryReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: 900}
	if code, _ := callTrustlineHistory(t, reader, "fiat:USD"); code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", code)
	}
}
