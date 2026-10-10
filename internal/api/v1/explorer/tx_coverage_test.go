package explorer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// txCoverageReader is a TransactionByHash-found ExplorerReader whose per-op
// result-code and contract-event sub-reads can be made to error independently,
// so TxDetail runs its non-fatal-degrade path (tx.go). Everything else returns
// harmless zero values (embedded capReader).
type txCoverageReader struct {
	*capReader
	resultsErr error
	eventsErr  error
}

func (r *txCoverageReader) TransactionByHash(context.Context, string) (clickhouse.TxSummary, bool, error) {
	return clickhouse.TxSummary{Seq: 42}, true, nil
}

func (r *txCoverageReader) OperationsByTx(context.Context, uint32, string) ([]clickhouse.OpRow, error) {
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

// txDetailBody drives TxDetail against a txCoverageReader and returns the
// marshalled wire body. It captures the JSON bytes (not the Go struct) so the
// assertion is on the WIRE CONTRACT — the exact surface coverage_note is about — and
// stays valid whether or not the response type carries a coverage_note field.
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

// TestTxDetail_FailedSubReadSurfacesCoverageNote is the regression guard: a
// FAILED per-op-result-code read and a FAILED contract-event read must each
// surface a non-empty coverage_note distinct from a transaction that genuinely
// had none. Both `events` and each op's `result_code` are omitempty, so without a
// note a failed read serialises byte-identically to a real empty — this test
// pins that they differ.
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

// TestTxDetail_FeeBumpByInnerHash pins the fee-bump contract of GET
// /v1/tx/{hash}: the inner hash (what the submitter's SDK returned) resolves
// to the transaction with its operations, max_fee is the payer's bid that
// fee_charged is bounded by, the payer and the inner failure reason are
// served, and a failed op says why rather than only "op_inner".
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

// TestTxDetail_SubReadDeadlineIs503NotAPartial202OK pins the boundary
// between the two things a failed sub-read can mean on /v1/tx/{hash}.
//
// A plain read failure is the non-fatal class: serve the transaction
// without per-op result codes or contract events, and say so in
// coverage_note. A blown DEADLINE is not that class. The budget
// belongs to the whole request, so every remaining sub-read is already
// doomed, and the "partial" 200 assembled out of nothing but failures
// tells the caller the transaction emitted no events — a wrong answer
// served with full confidence, where the truthful answer is "retry".
//
// The two fatal reads above these already take that branch; this pins
// that the non-fatal pair now agrees with them.
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
