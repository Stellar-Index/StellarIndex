//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestSponsorsRollup_DistinctSponsoredTotalIsGlobal is RLT-028's proof,
// run through the real cycle on a real ClickHouse. The board's
// distinct_sponsored_total must count each sponsored account once
// across the WHOLE board, not once per sponsor that touched it.
//
// Fixture: two different sponsors, each begin-then-end sponsoring the
// SAME account in its own transaction. Per-sponsor distinct_sponsored
// is 1 for each sponsor (correct: each sponsor covered one account),
// so summing it across sponsors gives 2 — the pre-fix bug. The account
// is in fact sponsored by two sponsors, so the true distinct count
// over the whole board is 1.
func TestSponsorsRollup_DistinctSponsoredTotalIsGlobal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const ledger = uint32(50)
	sponsorA := gAccountFromSeed(t, 0x41)
	sponsorB := gAccountFromSeed(t, 0x42)
	sponsored := gAccountFromSeed(t, 0x43)
	// Early close time: a later one becomes the lake's max(close_time) and
	// shifts NetworkThroughput's window off TestNetworkThroughput_* fixtures.
	closeTime := time.Date(2024, 1, 1, 0, 0, 50, 0, time.UTC)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	txHashA := "sponsdistinctaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	txHashB := "sponsdistinctbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	if err := sink.Add(ctx, chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime,
			LedgerHash: "aa04", PrevHash: "bb04", ProtocolVersion: 23, BucketListHash: "cc04",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		},
		Txs: []chstore.TransactionRow{
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashA, TxIndex: 0,
				SourceAccount: sponsorA, FeeCharged: 100, MaxFee: 100, OperationCount: 2, Successful: 1,
			},
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashB, TxIndex: 1,
				SourceAccount: sponsorB, FeeCharged: 100, MaxFee: 100, OperationCount: 2, Successful: 1,
			},
		},
		Ops: []chstore.OperationRow{
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashA, TxIndex: 0, OpIndex: 0,
				OpType: "OperationTypeBeginSponsoringFutureReserves", SourceAccount: sponsorA,
			},
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashA, TxIndex: 0, OpIndex: 1,
				OpType: "OperationTypeEndSponsoringFutureReserves", SourceAccount: sponsored,
			},
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashB, TxIndex: 1, OpIndex: 0,
				OpType: "OperationTypeBeginSponsoringFutureReserves", SourceAccount: sponsorB,
			},
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashB, TxIndex: 1, OpIndex: 1,
				OpType: "OperationTypeEndSponsoringFutureReserves", SourceAccount: sponsored,
			},
		},
	}); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	if err := chstore.RunSponsorsRollup(ctx, addr, t.Logf); err != nil {
		t.Fatalf("RunSponsorsRollup: %v", err)
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	board, ok, err := reader.AccountSponsors(ctx, 10, "")
	if err != nil {
		t.Fatalf("AccountSponsors: %v", err)
	}
	if !ok {
		t.Fatal("AccountSponsors: no board")
	}
	if got, want := board.DistinctSponsoredTotal, int64(1); got != want {
		t.Fatalf("DistinctSponsoredTotal = %d, want %d (one account sponsored by two sponsors "+
			"counts once across the whole board, not once per sponsor)", got, want)
	}
}
