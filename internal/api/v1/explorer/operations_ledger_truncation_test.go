package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// ledgerOpsTotalReader feeds GET /v1/operations?ledger=N a fixed page of
// rows and a controllable ledger header, to exercise the Total/Truncated
// signal independently of the response-byte-budget test's row content.
type ledgerOpsTotalReader struct {
	*capReader
	rows []clickhouse.OpRow
	hdr  clickhouse.LedgerHeader
}

func (r *ledgerOpsTotalReader) OperationsByLedger(ctx context.Context, _ uint32, _ int) ([]clickhouse.OpRow, error) {
	r.probe.record(ctx)
	return r.rows, nil
}

func (r *ledgerOpsTotalReader) LedgerBySeq(ctx context.Context, _ uint32) (clickhouse.LedgerHeader, bool, error) {
	r.probe.record(ctx)
	return r.hdr, true, nil
}

// TestOperations_LedgerPath_SignalsTruncation pins GH-1135: a ledger whose
// true operation count (from the ledger header) exceeds the served page
// must say so — the same Total/Truncated shape LedgerTransactionsView
// already gives /v1/ledgers/{seq}/transactions — rather than returning a
// silently short page with HTTP 200 and no way to tell "that's everything"
// from "that's a truncated slice".
func TestOperations_LedgerPath_SignalsTruncation(t *testing.T) {
	rows := []clickhouse.OpRow{
		{Seq: 64490439, CloseTime: time.Unix(1700000000, 0).UTC(), TxHash: "h0"},
		{Seq: 64490439, CloseTime: time.Unix(1700000000, 0).UTC(), TxHash: "h1"},
	}
	reader := &ledgerOpsTotalReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		rows:      rows,
		hdr:       clickhouse.LedgerHeader{OpCount: 1135}, // real ledger 64490439 example from the issue
	}
	h := newProbeHandler(reader, nil)
	var captured OperationsView
	h.WriteJSON = func(w http.ResponseWriter, data any, _ bool) {
		if v, ok := data.(OperationsView); ok {
			captured = v
		}
		w.WriteHeader(http.StatusOK)
	}

	rec := httptest.NewRecorder()
	h.Operations(rec, httptest.NewRequest(http.MethodGet, "/v1/operations?ledger=64490439", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if captured.Total != 1135 {
		t.Errorf("Total = %d, want 1135", captured.Total)
	}
	if !captured.Truncated {
		t.Error("Truncated = false, want true: served 2 of 1135 operations with no signal")
	}
}

// TestOperations_LedgerPath_FullPageNotTruncated is the non-triggering
// control: when the header's op count matches the served rows exactly,
// Truncated must stay false.
func TestOperations_LedgerPath_FullPageNotTruncated(t *testing.T) {
	rows := []clickhouse.OpRow{
		{Seq: 42, CloseTime: time.Unix(1700000000, 0).UTC(), TxHash: "h0"},
	}
	reader := &ledgerOpsTotalReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		rows:      rows,
		hdr:       clickhouse.LedgerHeader{OpCount: 1},
	}
	h := newProbeHandler(reader, nil)
	var captured OperationsView
	h.WriteJSON = func(w http.ResponseWriter, data any, _ bool) {
		if v, ok := data.(OperationsView); ok {
			captured = v
		}
		w.WriteHeader(http.StatusOK)
	}

	rec := httptest.NewRecorder()
	h.Operations(rec, httptest.NewRequest(http.MethodGet, "/v1/operations?ledger=42", nil))
	if captured.Truncated {
		t.Error("Truncated = true, want false: served operations equal the header's exact count")
	}
	if captured.Total != 1 {
		t.Errorf("Total = %d, want 1", captured.Total)
	}
}
