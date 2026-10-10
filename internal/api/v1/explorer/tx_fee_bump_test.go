package explorer

import (
	"context"

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
