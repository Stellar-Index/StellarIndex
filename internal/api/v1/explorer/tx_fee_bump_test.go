package explorer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

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

	if got.Hash != feeBumpOuterHash || got.MaxFee != 20_000 || got.FeeCharged != 2_000 {
		t.Fatalf("hash/max_fee/fee_charged = %s/%d/%d, want outer hash / 20000 / 2000", got.Hash, got.MaxFee, got.FeeCharged)
	}
	fb := got.FeeBump
	if fb == nil {
		t.Fatal("fee_bump absent on a fee-bump transaction")
	}
	if fb.FeeAccount != feeBumpPayer || fb.InnerHash != feeBumpInnerHash || fb.InnerMaxFee != 100 {
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

// TestTxSummaryView_NotFeeBump: an ordinary transaction keeps its own max_fee
// and carries no fee_bump object.
func TestTxSummaryView_NotFeeBump(t *testing.T) {
	v := txSummaryView(clickhouse.TxSummary{TxHash: feeBumpOuterHash, MaxFee: 300, FeeCharged: 100})
	if v.MaxFee != 300 || v.FeeBump != nil {
		t.Fatalf("plain tx view = max_fee %d fee_bump %+v, want 300 / nil", v.MaxFee, v.FeeBump)
	}
}
