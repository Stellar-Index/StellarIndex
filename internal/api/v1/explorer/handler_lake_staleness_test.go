package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// GH-1067: a wedged lake (LakeWatermark reporting stale) must be reflected
// in flags.stale for every explorer handler, not just the handful that
// already consulted it (account_state.go, contracts.go, operations.go,
// movements.go). Before this fix LedgersList (and its siblings sharing the
// same hardcoded WriteJSON(w, out, false)) served a wedged lake as fresh.
func TestLedgersList_ReflectsLakeStaleness(t *testing.T) {
	probe := &deadlineProbe{}
	h := newProbeHandler(&capReader{probe: probe}, nil)
	h.LakeWatermark = func(_ context.Context) (uint32, bool, bool) { return 100, true, true }

	var gotStale bool
	var staleSet bool
	h.WriteJSON = func(w http.ResponseWriter, _ any, stale bool) {
		gotStale, staleSet = stale, true
		w.WriteHeader(http.StatusOK)
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/ledgers", nil)
	rec := httptest.NewRecorder()
	h.LedgersList(rec, r)

	if !staleSet {
		t.Fatalf("WriteJSON was never called")
	}
	if !gotStale {
		t.Fatalf("LedgersList served flags.stale=false while LakeWatermark reported the lake stale")
	}
}

// lakeFreshReader answers the rollup and wasm reads that capReader reports as
// warming/unresolved, so those handlers reach their 200 path.
type lakeFreshReader struct{ *capReader }

func (lakeFreshReader) AccountsStats(context.Context) (clickhouse.AccountsStats, bool, error) {
	return clickhouse.AccountsStats{}, true, nil
}

func (lakeFreshReader) AccountGraphHistory(context.Context, string) (clickhouse.AccountGraphHistory, bool, error) {
	return clickhouse.AccountGraphHistory{}, true, nil
}

func (lakeFreshReader) ContractWasm(context.Context, string) (clickhouse.ContractWasmInfo, error) {
	return clickhouse.ContractWasmInfo{}, nil
}

func TestLakeBackedHandlers_ReflectLakeStaleness(t *testing.T) {
	cases := []struct {
		name   string
		reader ExplorerReader
		serve  func(h *Handler, w http.ResponseWriter, r *http.Request)
		target string
		path   map[string]string
	}{
		{"ledger operations route", nil, (*Handler).LedgerOperations, "/v1/ledgers/5/operations", map[string]string{"seq": "5"}},
		{"operations cursor page", nil, (*Handler).Operations, "/v1/operations?cursor=63000000.1.0", nil},
		{
			"contract events cursor page", nil, (*Handler).ContractDetail,
			"/v1/contracts/x?cursor=5." + strings.Repeat("a", 64) + ".0.0",
			map[string]string{"contract_id": validTestContract},
		},
		{
			"contract wasm", nil, (*Handler).ContractWasm, "/v1/contracts/x/wasm",
			map[string]string{"contract_id": validTestContract},
		},
		{"accounts stats", nil, (*Handler).AccountsStats, "/v1/accounts/stats", nil},
		{
			"account graph history", nil, (*Handler).AccountGraphHistory, "/v1/accounts/x/graph/history",
			map[string]string{"g_strkey": validTestAccount},
		},
		{
			"tx detail", &txCoverageReader{capReader: &capReader{probe: &deadlineProbe{}}}, (*Handler).TxDetail,
			"/v1/tx/x",
			map[string]string{"hash": strings.Repeat("b", 64)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := tc.reader
			if reader == nil {
				reader = lakeFreshReader{&capReader{probe: &deadlineProbe{}}}
			}
			h := newProbeHandler(reader, nil)
			h.LakeWatermark = func(context.Context) (uint32, bool, bool) { return 100, true, true }
			var gotStale, staleSet bool
			h.WriteJSON = func(w http.ResponseWriter, _ any, stale bool) {
				gotStale, staleSet = stale, true
				w.WriteHeader(http.StatusOK)
			}
			r := httptest.NewRequest(http.MethodGet, tc.target, nil)
			for k, v := range tc.path {
				r.SetPathValue(k, v)
			}
			rec := httptest.NewRecorder()
			tc.serve(h, rec, r)
			if !staleSet {
				t.Fatalf("WriteJSON was never called (status %d)", rec.Code)
			}
			if !gotStale {
				t.Fatalf("served flags.stale=false while LakeWatermark reported the lake stale")
			}
		})
	}
}

func TestTxDetail_NotFoundDetailNamesLakeLag(t *testing.T) {
	cases := []struct {
		name       string
		tip        uint32
		stale, ok  bool
		wantSubstr string
	}{
		{"fresh", 100, false, true, "in the indexed range"},
		{"stale with tip", 64000000, true, true, "ends at ledger 64000000 and is behind the network"},
		{"stale without tip", 0, true, false, "freshness is unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newProbeHandler(&capReader{probe: &deadlineProbe{}}, nil)
			h.LakeWatermark = func(context.Context) (uint32, bool, bool) { return tc.tip, tc.stale, tc.ok }
			var detail string
			h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, d string) {
				detail = d
				w.WriteHeader(status)
			}
			r := httptest.NewRequest(http.MethodGet, "/v1/tx/x", nil)
			r.SetPathValue("hash", strings.Repeat("c", 64))
			rec := httptest.NewRecorder()
			h.TxDetail(rec, r)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if !strings.Contains(detail, tc.wantSubstr) {
				t.Fatalf("detail = %q, want it to contain %q", detail, tc.wantSubstr)
			}
			if !tc.stale && strings.Contains(detail, "behind") {
				t.Fatalf("fresh-lake detail claims lag: %q", detail)
			}
		})
	}
}
