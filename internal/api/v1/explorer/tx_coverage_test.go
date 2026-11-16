package explorer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// txCoverageReader is an ExplorerReader whose TransactionByHash succeeds and whose per-op result-code and contract-event
// sub-reads can error independently, driving TxDetail's non-fatal degrade path; the rest returns zero values.
type txCoverageReader struct {
	*capReader
	resultsErr error
	eventsErr  error
	// uncommitted: the tx's ledger has no stellar.ledgers row yet (the sink writes it last).
	uncommitted bool
	calls       []string
}

func (r *txCoverageReader) LedgerBySeq(context.Context, uint32) (clickhouse.LedgerHeader, bool, error) {
	r.calls = append(r.calls, "LedgerBySeq")
	return clickhouse.LedgerHeader{TxCount: 1, OpCount: 1}, !r.uncommitted, nil
}

func (r *txCoverageReader) LedgerTransactions(context.Context, uint32, int) ([]clickhouse.TxSummary, error) {
	r.calls = append(r.calls, "LedgerTransactions")
	return nil, nil
}

func (r *txCoverageReader) OperationsByLedger(context.Context, uint32, int) ([]clickhouse.OpRow, error) {
	r.calls = append(r.calls, "OperationsByLedger")
	return nil, nil
}

func (r *txCoverageReader) TransactionByHash(context.Context, string) (clickhouse.TxSummary, bool, error) {
	return clickhouse.TxSummary{Seq: 42}, true, nil
}

func (r *txCoverageReader) OperationsByTx(context.Context, uint32, string) ([]clickhouse.OpRow, error) {
	r.calls = append(r.calls, "OperationsByTx")
	return nil, nil
}

func (r *txCoverageReader) OperationResultsByTx(context.Context, uint32, string) (map[uint32]clickhouse.OpResult, error) {
	if r.resultsErr != nil {
		return nil, r.resultsErr
	}
	return map[uint32]clickhouse.OpResult{}, nil
}

func (r *txCoverageReader) EventsByTx(context.Context, uint32, string) ([]clickhouse.EventSummary, error) {
	if r.eventsErr != nil {
		return nil, r.eventsErr
	}
	return nil, nil
}

// Drives TxDetail against a txCoverageReader and returns the marshalled body, so assertions are on the wire contract
// (what coverage_note describes) whether or not the response type carries a coverage_note field.
func txDetailBody(t *testing.T, resultsErr, eventsErr error) map[string]any {
	t.Helper()
	var captured []byte
	h := &Handler{
		Reader:        &txCoverageReader{capReader: &capReader{probe: &deadlineProbe{}}, resultsErr: resultsErr, eventsErr: eventsErr},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClientAborted: func(*http.Request, error) bool { return false },
		WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
			w.WriteHeader(status)
		},
		WriteJSON: func(w http.ResponseWriter, data any, _ bool) {
			b, err := json.Marshal(data)
			if err != nil {
				t.Fatalf("marshalling TxDetail response: %v", err)
			}
			captured = b
			_, _ = w.Write(b)
		},
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/tx/"+validTestTxHash, nil)
	r.SetPathValue("hash", validTestTxHash)
	rec := httptest.NewRecorder()
	h.TxDetail(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("TxDetail returned status %d, want 200 (the reads degrade, they must not fail the request)", rec.Code)
	}
	if captured == nil {
		t.Fatal("TxDetail never wrote a JSON body")
	}
	var out map[string]any
	if err := json.Unmarshal(captured, &out); err != nil {
		t.Fatalf("unmarshalling captured body: %v", err)
	}
	return out
}

// A failed sub-read must also drop the route's shared-cache band, or a CDN
// replays the partial tx for the band's whole s-maxage after the read recovers.
func TestTxDetail_FailedSubReadDropsCacheBand(t *testing.T) {
	const band = "public, max-age=60, s-maxage=300"
	readErr := errors.New("clickhouse read failed")
	for _, tc := range []struct {
		name                  string
		resultsErr, eventsErr error
		want                  string
	}{
		{"both reads succeed", nil, nil, band},
		{"results read fails", readErr, nil, "no-store"},
		{"events read fails", nil, readErr, "no-store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{
				Reader:        &txCoverageReader{capReader: &capReader{probe: &deadlineProbe{}}, resultsErr: tc.resultsErr, eventsErr: tc.eventsErr},
				Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				ClientAborted: func(*http.Request, error) bool { return false },
				WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
					w.WriteHeader(status)
				},
				WriteJSON: func(w http.ResponseWriter, _ any, _ bool) { w.WriteHeader(http.StatusOK) },
				WriteJSONAt: func(w http.ResponseWriter, _ any, _, degraded bool, _ time.Time) {
					if degraded {
						w.Header().Set("Cache-Control", "no-store")
					}
					w.WriteHeader(http.StatusOK)
				},
			}
			r := httptest.NewRequest(http.MethodGet, "/v1/tx/"+validTestTxHash, nil)
			r.SetPathValue("hash", validTestTxHash)
			rec := httptest.NewRecorder()
			rec.Header().Set("Cache-Control", band)
			h.TxDetail(rec, r)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Cache-Control"); got != tc.want {
				t.Errorf("Cache-Control = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTxDetail_FailedSubReadSurfacesCoverageNote: a FAILED per-op-result-code read and a FAILED
// contract-event read must each surface a coverage_note. `events` and each op's `result_code` are
// omitempty, so without a note a failed read serialises identically to a real empty.
func TestTxDetail_FailedSubReadSurfacesCoverageNote(t *testing.T) {
	readErr := errors.New("clickhouse read failed")

	// Baseline: both sub-reads succeed and are genuinely empty. This is the
	// "genuinely had none" wire shape the failure cases must be distinguishable
	// from — it MUST carry no coverage_note.
	genuinelyEmpty := txDetailBody(t, nil, nil)
	if note, present := genuinelyEmpty["coverage_note"]; present {
		t.Fatalf("a genuinely-empty tx must not carry a coverage_note, got %q", note)
	}

	cases := []struct {
		name       string
		resultsErr error
		eventsErr  error
		wantSubstr string
	}{
		{"results read fails", readErr, nil, "result_code"},
		{"events read fails", nil, readErr, "events"},
		{"both reads fail", readErr, readErr, "result_code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := txDetailBody(t, tc.resultsErr, tc.eventsErr)

			noteRaw, present := body["coverage_note"]
			if !present {
				t.Fatalf("a failed sub-read must surface a coverage_note, but the field is absent — "+
					"the failure is byte-identical to a genuinely-empty tx (W1.2). body=%v", body)
			}
			note, _ := noteRaw.(string)
			if note == "" {
				t.Fatalf("coverage_note present but empty — a failed read must be distinguishable from empty (W1.2)")
			}
			// Non-vacuous: the failing response must actually DIFFER on the wire
			// from the genuinely-empty one.
			if _, ok := genuinelyEmpty["coverage_note"]; ok {
				t.Fatal("baseline unexpectedly carried a coverage_note")
			}
			if !strings.Contains(strings.ToLower(note), strings.ToLower(tc.wantSubstr)) {
				t.Fatalf("coverage_note %q does not mention the degraded field %q", note, tc.wantSubstr)
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

// TestTxDetail_FeeBumpByInnerHash pins the fee-bump contract of GET /v1/tx/{hash}: the inner hash
// resolves to the transaction with its operations, max_fee is the payer's bid bounding
// fee_charged, the payer and inner failure reason are served, and a failed op says why.
func TestTxDetail_FeeBumpByInnerHash(t *testing.T) {
	underfunded, err := xdr.MarshalBase64(xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type:          xdr.OperationTypePayment,
			PaymentResult: &xdr.PaymentResult{Code: xdr.PaymentResultCodePaymentUnderfunded},
		},
	})
	if err != nil {
		t.Fatalf("marshal op result: %v", err)
	}
	var captured []byte
	h := &Handler{
		Reader:        &feeBumpReader{capReader: &capReader{probe: &deadlineProbe{}}, opResultXDR: underfunded},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClientAborted: func(*http.Request, error) bool { return false },
		WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
			w.WriteHeader(status)
		},
		WriteJSON: func(w http.ResponseWriter, data any, _ bool) {
			b, err := json.Marshal(data)
			if err != nil {
				t.Fatalf("marshal TxDetail: %v", err)
			}
			captured = b
			_, _ = w.Write(b)
		},
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/tx/"+feeBumpInnerHash, nil)
	r.SetPathValue("hash", feeBumpInnerHash)
	rec := httptest.NewRecorder()
	h.TxDetail(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var got TxDetailView
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Hash != feeBumpOuterHash || got.MaxFee != "20000" || got.FeeCharged != "2000" {
		t.Fatalf("hash/max_fee/fee_charged = %s/%s/%s, want outer hash / 20000 / 2000", got.Hash, got.MaxFee, got.FeeCharged)
	}
	fb := got.FeeBump
	if fb == nil {
		t.Fatal("fee_bump absent on a fee-bump transaction")
	}
	if fb.FeeAccount != feeBumpPayer || fb.InnerHash != feeBumpInnerHash || fb.InnerMaxFee != "100" {
		t.Fatalf("fee_bump = %+v, want payer / inner hash / inner max fee 100", *fb)
	}
	if fb.InnerResultCode == nil || *fb.InnerResultCode != int32(xdr.TransactionResultCodeTxFailed) || fb.InnerResult != "tx_failed" {
		t.Fatalf("fee_bump inner result = %v/%q, want -1/tx_failed", fb.InnerResultCode, fb.InnerResult)
	}
	if len(got.Operations) != 1 {
		t.Fatalf("operations = %d, want 1 (sub-reads must use the outer hash)", len(got.Operations))
	}
	if op := got.Operations[0]; op.Result != "op_inner" || op.InnerResult != "payment_underfunded" {
		t.Fatalf("op result = %q / inner %q, want op_inner / payment_underfunded", op.Result, op.InnerResult)
	}
}

// Pins the two meanings of a failed sub-read on /v1/tx/{hash}. A plain failure is non-fatal: serve the transaction
// without per-op result codes or events and say so in coverage_note. A blown deadline is not: the budget is request-
// wide, so every remaining sub-read is doomed and a partial 200 would claim no events; the truthful answer is 503 retry.
func TestTxDetail_SubReadDeadlineIs503NotAPartial200OK(t *testing.T) {
	cases := map[string]struct{ resultsErr, eventsErr error }{
		"result-code read hits the deadline": {context.DeadlineExceeded, nil},
		"event read hits the deadline":       {nil, context.DeadlineExceeded},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := &Handler{
				Reader: &txCoverageReader{
					capReader:  &capReader{probe: &deadlineProbe{}},
					resultsErr: tc.resultsErr,
					eventsErr:  tc.eventsErr,
				},
				Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				ClientAborted: func(*http.Request, error) bool { return false },
				WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
					w.WriteHeader(status)
				},
				WriteJSON: func(w http.ResponseWriter, _ any, _ bool) {
					w.WriteHeader(http.StatusOK)
				},
			}
			r := httptest.NewRequest(http.MethodGet, "/v1/tx/"+validTestTxHash, nil)
			r.SetPathValue("hash", validTestTxHash)
			rec := httptest.NewRecorder()
			h.TxDetail(rec, r)

			if rec.Code == http.StatusOK {
				t.Fatalf("status = 200 — a sub-read that blew the request deadline must not be " +
					"served as a partial transaction; the caller cannot tell it from a tx that " +
					"genuinely emitted nothing")
			}
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (retryable, same branch the fatal reads take)", rec.Code)
			}
		})
	}
}

const (
	feeBumpOuterHash = "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"
	feeBumpInnerHash = "1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e"
	feeBumpPayer     = "GAFEEPAYERFEEPAYERFEEPAYERFEEPAYERFEEPAYERFEEPAYERFEEP"
)

// feeBumpReader serves one fee bump the way the lake stores it: the row is
// found by either hash, but its operations and results are keyed on the
// OUTER hash only.
type feeBumpReader struct {
	*capReader
	opResultXDR string
}

func (r *feeBumpReader) TransactionByHash(_ context.Context, hash string) (clickhouse.TxSummary, bool, error) {
	if hash != feeBumpOuterHash && hash != feeBumpInnerHash {
		return clickhouse.TxSummary{}, false, nil
	}
	return clickhouse.TxSummary{
		Seq: 42, TxHash: feeBumpOuterHash, SourceAccount: "GINNERSOURCE",
		FeeCharged: 2_000, MaxFee: 100, OperationCount: 1,
		ResultCode:  int32(xdr.TransactionResultCodeTxFeeBumpInnerFailed),
		InnerTxHash: feeBumpInnerHash, FeeAccount: feeBumpPayer, FeeBumpFee: 20_000,
		InnerResultCode: int32(xdr.TransactionResultCodeTxFailed),
	}, true, nil
}

func (r *feeBumpReader) OperationsByTx(_ context.Context, _ uint32, hash string) ([]clickhouse.OpRow, error) {
	if hash != feeBumpOuterHash {
		return nil, nil
	}
	return []clickhouse.OpRow{{Seq: 42, TxHash: feeBumpOuterHash, OpType: "OperationTypePayment", BodyXDR: "not-valid-xdr"}}, nil
}

func (r *feeBumpReader) OperationResultsByTx(_ context.Context, _ uint32, hash string) (map[uint32]clickhouse.OpResult, error) {
	if hash != feeBumpOuterHash {
		return map[uint32]clickhouse.OpResult{}, nil
	}
	return map[uint32]clickhouse.OpResult{0: {Code: int32(xdr.OperationResultCodeOpInner), ResultXDR: r.opResultXDR}}, nil
}

func (r *feeBumpReader) EventsByTx(context.Context, uint32, string) ([]clickhouse.EventSummary, error) {
	return nil, nil
}

// The sink writes a ledger's stellar.ledgers row after every child table. A read of a ledger without that row may see
// child tables that are not written yet, so its empties are not real empties: the response says so and leaves the
// closed-ledger cache band. The marker is read BEFORE the child tables; read after, a flush could land in between.
func TestLedgerChildReads_DiscloseAnUncommittedLedger(t *testing.T) {
	const band = "public, max-age=60, s-maxage=300"
	for _, tc := range []struct {
		name      string
		serve     func(*Handler, http.ResponseWriter, *http.Request)
		target    string
		path      map[string]string
		childRead string
		hasNote   bool
	}{
		{"tx detail", (*Handler).TxDetail, "/v1/tx/" + validTestTxHash, map[string]string{"hash": validTestTxHash}, "OperationsByTx", true},
		{"ledger operations", (*Handler).LedgerOperations, "/v1/ledgers/42/operations", map[string]string{"seq": "42"}, "OperationsByLedger", true},
		{"ledger transactions", (*Handler).LedgerTransactions, "/v1/ledgers/42/transactions", map[string]string{"seq": "42"}, "LedgerTransactions", false},
	} {
		for _, uncommitted := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/uncommitted=%t", tc.name, uncommitted), func(t *testing.T) {
				reader := &txCoverageReader{capReader: &capReader{probe: &deadlineProbe{}}, uncommitted: uncommitted}
				var body []byte
				h := newProbeHandler(reader, nil)
				h.WriteJSON = func(w http.ResponseWriter, data any, _ bool) {
					body, _ = json.Marshal(data)
					w.WriteHeader(http.StatusOK)
				}
				h.WriteJSONAt = func(w http.ResponseWriter, data any, _, degraded bool, _ time.Time) {
					body, _ = json.Marshal(data)
					if degraded {
						w.Header().Set("Cache-Control", "no-store")
					}
					w.WriteHeader(http.StatusOK)
				}
				r := httptest.NewRequest(http.MethodGet, tc.target, nil)
				for k, v := range tc.path {
					r.SetPathValue(k, v)
				}
				rec := httptest.NewRecorder()
				rec.Header().Set("Cache-Control", band)
				tc.serve(h, rec, r)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", rec.Code)
				}

				marker, child := slices.Index(reader.calls, "LedgerBySeq"), slices.Index(reader.calls, tc.childRead)
				if marker < 0 || child < 0 || marker > child {
					t.Fatalf("reads = %v: the commit marker must be read before %s", reader.calls, tc.childRead)
				}
				wantCache := band
				if uncommitted {
					wantCache = "no-store"
				}
				if got := rec.Header().Get("Cache-Control"); got != wantCache {
					t.Errorf("Cache-Control = %q, want %q", got, wantCache)
				}
				var view map[string]any
				if err := json.Unmarshal(body, &view); err != nil {
					t.Fatalf("decode: %v", err)
				}
				note, _ := view["coverage_note"].(string)
				if want := uncommitted && tc.hasNote; (note != "") != want {
					t.Errorf("coverage_note = %q, want present=%t", note, want)
				}
				if uncommitted && tc.hasNote && !strings.Contains(note, "ledger 42 has no commit record") {
					t.Errorf("coverage_note = %q, want it to name the uncommitted ledger", note)
				}
			})
		}
	}
}
