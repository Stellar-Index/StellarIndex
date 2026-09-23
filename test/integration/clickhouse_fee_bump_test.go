//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestClickHouseFeeBumpRoundTrip drives a fee bump through the real Sink,
// tier1_schema.sql's tx_hash_index MVs and the ExplorerReader: the inner hash
// (what the submitter's SDK returned) must resolve to the outer row, the
// outer layer must survive write→read, the per-op result XDR must be served
// beside its outer code, and the windowed index backfill must re-index both
// hashes.
func TestClickHouseFeeBumpRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const (
		ledger       = uint32(73_000_001)
		outerHash    = "3333333333333333333333333333333333333333333333333333333333333333"
		innerHash    = "4444444444444444444444444444444444444444444444444444444444444444"
		payerAccount = "GFEEPAYERINTEGRATIONFIXTURE"
		opResultB64  = "AAAAAAAAAAH////+" // op_inner / payment / PAYMENT_UNDERFUNDED
	)
	closeTime := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{LedgerSeq: ledger, CloseTime: closeTime, TxCount: 1, OpCount: 1},
		Txs: []chstore.TransactionRow{{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: outerHash, TxIndex: 0,
			SourceAccount: "GINNERSOURCE", FeeCharged: 2_000, MaxFee: 100, OperationCount: 1,
			ResultCode: -13, MemoType: "MemoTypeMemoNone",
			InnerTxHash: innerHash, FeeAccount: payerAccount, FeeBumpFee: 20_000, InnerResultCode: -1,
		}},
		Results: []chstore.OperationResultRow{{
			LedgerSeq: ledger, TxHash: outerHash, OpIndex: 0, ResultCode: 0, ResultXDR: opResultB64,
		}},
	}
	if err := sink.Add(ctx, ext); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	assertResolves := func(label string, er *chstore.ExplorerReader) {
		t.Helper()
		for _, h := range []string{innerHash, outerHash} {
			tx, found, err := er.TransactionByHash(ctx, h)
			if err != nil || !found {
				t.Fatalf("%s: TransactionByHash(%s) = (found=%v, err=%v), want hit", label, h[:4], found, err)
			}
			if tx.TxHash != outerHash || tx.Seq != ledger || tx.InnerTxHash != innerHash ||
				tx.FeeAccount != payerAccount || tx.FeeBumpFee != 20_000 || tx.MaxFee != 100 || tx.InnerResultCode != -1 {
				t.Fatalf("%s: TransactionByHash(%s) = %+v, want the fee bump's outer row with its outer layer", label, h[:4], tx)
			}
		}
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	assertResolves("mv", er)

	results, err := er.OperationResultsByTx(ctx, ledger, outerHash)
	if err != nil {
		t.Fatalf("OperationResultsByTx: %v", err)
	}
	if got := results[0]; got.Code != 0 || got.ResultXDR != opResultB64 {
		t.Fatalf("op result = %+v, want code 0 with its result XDR", got)
	}

	// Re-derive the index from the base table alone: the backfill must
	// index the inner hash too, or a recreated index 404s every fee bump.
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.tx_hash_index`); err != nil {
		t.Fatalf("truncate tx_hash_index: %v", err)
	}
	if err := chstore.BackfillTxHashIndex(ctx, addr, ledger, ledger, 1, t.Logf); err != nil {
		t.Fatalf("backfill tx_hash_index: %v", err)
	}
	var indexed uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.tx_hash_index FINAL WHERE tx_hash IN (?, ?)`,
		outerHash, innerHash).Scan(&indexed); err != nil {
		t.Fatalf("count indexed hashes: %v", err)
	}
	if indexed != 2 {
		t.Fatalf("backfill indexed %d of the fee bump's 2 hashes", indexed)
	}
	er2, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader (backfilled): %v", err)
	}
	t.Cleanup(func() { _ = er2.Close() })
	assertResolves("backfill", er2)
}
