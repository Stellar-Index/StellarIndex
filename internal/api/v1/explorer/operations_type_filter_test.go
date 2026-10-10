package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type opTypeReader struct {
	*opsDirReader
	gotTypes []string
	gotCur   clickhouse.ExplorerCursor
	page     clickhouse.OpTypePage
}

func (r *opTypeReader) RecentOperationsOfType(_ context.Context, _ int, cur clickhouse.ExplorerCursor, opTypes []string) (clickhouse.OpTypePage, error) {
	r.gotTypes, r.gotCur = opTypes, cur
	return r.page, nil
}

func newOpTypeHandler(page clickhouse.OpTypePage) (*Handler, *opTypeReader, *writeCapture) {
	h, base, captured := newOpsDirHandler()
	reader := &opTypeReader{opsDirReader: base, page: page}
	h.Reader = reader
	return h, reader, captured
}

func TestOperations_TypeFilterMapsToLakeEnumAndBypassesDirectoryCache(t *testing.T) {
	h, reader, captured := newOpTypeHandler(clickhouse.OpTypePage{
		Rows: []clickhouse.OpRow{{
			Seq: 63_000_000, CloseTime: time.Unix(1700000000, 0).UTC(),
			TxHash: "h", OpType: "OperationTypePayment",
		}},
		ScannedFrom: 62_995_001,
	})
	rec := httptest.NewRecorder()
	h.Operations(rec, httptest.NewRequest(http.MethodGet,
		"/v1/operations?type=payment,manage_sell_offer&type=payment", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if want := []string{"OperationTypePayment", "OperationTypeManageSellOffer"}; !reflect.DeepEqual(reader.gotTypes, want) {
		t.Errorf("types = %v, want %v (deduplicated lake enum strings)", reader.gotTypes, want)
	}
	if n := reader.calls.Load(); n != 0 {
		t.Errorf("a filtered first page read the unfiltered directory %d times; it must not be served from that cache", n)
	}
	// A short page continues strictly below the scanned span, never stops.
	if got := captured.view.NextCursor; got != "62995001.0.0" {
		t.Errorf("next_cursor = %q, want the scan floor 62995001.0.0", got)
	}
	if captured.view.OpTypeStats != nil {
		t.Error("op_type_stats describes the unfiltered network; a filtered page must not carry it")
	}
}

func TestOperations_TypeFilterCursorPassesThrough(t *testing.T) {
	h, reader, captured := newOpTypeHandler(clickhouse.OpTypePage{})
	rec := httptest.NewRecorder()
	h.Operations(rec, httptest.NewRequest(http.MethodGet, "/v1/operations?type=clawback&cursor=4000.0.0", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if want := (clickhouse.ExplorerCursor{Ledger: 4000}); reader.gotCur != want {
		t.Errorf("cursor = %+v, want %+v", reader.gotCur, want)
	}
	if captured.view.NextCursor != "" {
		t.Errorf("next_cursor = %q, want none once the scan reached genesis", captured.view.NextCursor)
	}
}

func TestOperations_TypeFilterRejectsUnknownType(t *testing.T) {
	for _, q := range []string{"type=bogus", "type=payment,", "type=Payment", "type=OperationTypePayment"} {
		h, reader, _ := newOpTypeHandler(clickhouse.OpTypePage{})
		rec := httptest.NewRecorder()
		h.Operations(rec, httptest.NewRequest(http.MethodGet, "/v1/operations?"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
		}
		if reader.gotTypes != nil {
			t.Errorf("%s: reached the lake with %v", q, reader.gotTypes)
		}
	}
}

// ParseLimit(500, 2000) bounds ROW count but not the response's BYTE
// size — an op's decoded body is attacker-influenced in size. Past
// operationsResponseByteBudget, further rows must be served UNDECODED
// (RawXDR empty) rather than fully decoded, bounding the response to
// roughly the budget regardless of how many giant-bodied ops a ledger has.
func TestOperations_LedgerPath_BoundsDecodedResponseBytes(t *testing.T) {
	half := operationsResponseByteBudget / 2
	rows := []clickhouse.OpRow{
		{Seq: 42, CloseTime: time.Unix(1700000000, 0).UTC(), TxHash: "h0", BodyXDR: garbageXDR(half)},        // fits: decoded
		{Seq: 42, CloseTime: time.Unix(1700000000, 0).UTC(), TxHash: "h1", BodyXDR: garbageXDR(half + 1024)}, // would exceed budget: light
		{Seq: 42, CloseTime: time.Unix(1700000000, 0).UTC(), TxHash: "h2", BodyXDR: garbageXDR(10)},          // still fits under budget: decoded
	}
	reader := &ledgerOpsReader{capReader: &capReader{probe: &deadlineProbe{}}, rows: rows}
	h := newProbeHandler(reader, nil)
	var captured OperationsView
	h.WriteJSON = func(w http.ResponseWriter, data any, _ bool) {
		if v, ok := data.(OperationsView); ok {
			captured = v
		}
		w.WriteHeader(http.StatusOK)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/ledgers/42/operations", nil)
	req.SetPathValue("seq", "42")
	h.LedgerOperations(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(captured.Operations) != 3 {
		t.Fatalf("got %d operations, want 3 (rows are degraded, never dropped)", len(captured.Operations))
	}
	if captured.Operations[0].RawXDR != rows[0].BodyXDR {
		t.Errorf("row 0 (fits the budget) was not fully decoded: RawXDR = %d bytes, want %d",
			len(captured.Operations[0].RawXDR), len(rows[0].BodyXDR))
	}
	if captured.Operations[1].RawXDR != "" {
		t.Errorf("row 1 (would exceed the budget) was fully decoded anyway: RawXDR = %d bytes, want 0 (light)",
			len(captured.Operations[1].RawXDR))
	}
	if captured.Operations[2].RawXDR != rows[2].BodyXDR {
		t.Errorf("row 2 (still under budget after row 1 was skipped) was not decoded: RawXDR = %d bytes, want %d",
			len(captured.Operations[2].RawXDR), len(rows[2].BodyXDR))
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

// TestOperations_LedgerPath_SignalsTruncation pins that a ledger whose
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
	req := httptest.NewRequest(http.MethodGet, "/v1/ledgers/64490439/operations", nil)
	req.SetPathValue("seq", "64490439")
	h.LedgerOperations(rec, req)
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
	req := httptest.NewRequest(http.MethodGet, "/v1/ledgers/42/operations", nil)
	req.SetPathValue("seq", "42")
	h.LedgerOperations(rec, req)
	if captured.Truncated {
		t.Error("Truncated = true, want false: served operations equal the header's exact count")
	}
	if captured.Total != 1 {
		t.Errorf("Total = %d, want 1", captured.Total)
	}
}
