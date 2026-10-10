package explorer

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// serveLedgerOps runs one request through serve and returns the OperationsView
// handed to WriteJSON.
func serveLedgerOps(t *testing.T, reader ExplorerReader, serve func(*Handler, http.ResponseWriter, *http.Request), target, seq string) OperationsView {
	t.Helper()
	h := newProbeHandler(reader, nil)
	var captured OperationsView
	h.WriteJSON = func(w http.ResponseWriter, data any, _ bool) {
		if v, ok := data.(OperationsView); ok {
			captured = v
		}
		w.WriteHeader(http.StatusOK)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if seq != "" {
		req.SetPathValue("seq", seq)
	}
	rec := httptest.NewRecorder()
	serve(h, rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200", target, rec.Code)
	}
	return captured
}

func TestLedgerOperations_SignalsTruncation(t *testing.T) {
	reader := &ledgerOpsTotalReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		rows: []clickhouse.OpRow{
			{Seq: 64490439, CloseTime: time.Unix(1700000000, 0).UTC(), TxHash: "h0"},
			{Seq: 64490439, CloseTime: time.Unix(1700000000, 0).UTC(), TxHash: "h1"},
		},
		hdr: clickhouse.LedgerHeader{OpCount: 1135},
	}
	got := serveLedgerOps(t, reader, (*Handler).LedgerOperations, "/v1/ledgers/64490439/operations", "64490439")
	if got.Ledger != 64490439 {
		t.Errorf("Ledger = %d, want 64490439", got.Ledger)
	}
	if got.Total != 1135 {
		t.Errorf("Total = %d, want 1135", got.Total)
	}
	if !got.Truncated {
		t.Error("Truncated = false, want true: served 2 of 1135 operations with no signal")
	}
}

func TestLedgerOperations_InvalidSeq400(t *testing.T) {
	for _, seq := range []string{"abc", "4294967296"} {
		h := newProbeHandler(&capReader{probe: &deadlineProbe{}}, nil)
		var problemType string
		h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, typ, _ string, status int, _ string) {
			problemType = typ
			w.WriteHeader(status)
		}
		req := httptest.NewRequest(http.MethodGet, "/v1/ledgers/"+seq+"/operations", nil)
		req.SetPathValue("seq", seq)
		rec := httptest.NewRecorder()
		h.LedgerOperations(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("seq=%q: status = %d, want 400", seq, rec.Code)
		}
		if problemType != "https://api.stellarindex.io/errors/invalid-ledger" {
			t.Errorf("seq=%q: problem type = %q, want invalid-ledger", seq, problemType)
		}
	}
}
