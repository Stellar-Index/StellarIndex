package explorer

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// /v1/operations is the directory only: any ?ledger= form, including the
// empty and zero values, must be refused with a pointer to the per-ledger
// route rather than served as a directory page.
func TestOperations_LedgerParamRejected(t *testing.T) {
	for _, q := range []string{"42", "", "0", "abc"} {
		probe := &deadlineProbe{}
		h := newProbeHandler(&capReader{probe: probe}, nil)
		var problemType, detail string
		h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, typ, _ string, status int, d string) {
			problemType, detail = typ, d
			w.WriteHeader(status)
		}
		h.WriteJSON = func(w http.ResponseWriter, _ any, _ bool) {
			t.Errorf("ledger=%q: served a 200 body", q)
			w.WriteHeader(http.StatusOK)
		}
		rec := httptest.NewRecorder()
		h.Operations(rec, httptest.NewRequest(http.MethodGet, "/v1/operations?ledger="+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("ledger=%q: status = %d, want 400", q, rec.Code)
		}
		if problemType != "https://api.stellarindex.io/errors/invalid-parameter" {
			t.Errorf("ledger=%q: problem type = %q, want invalid-parameter", q, problemType)
		}
		if !strings.Contains(detail, "/v1/ledgers/{seq}/operations") {
			t.Errorf("ledger=%q: detail = %q, want a pointer to /v1/ledgers/{seq}/operations", q, detail)
		}
		if probe.sawCall {
			t.Errorf("ledger=%q: reached the lake before refusing", q)
		}
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
