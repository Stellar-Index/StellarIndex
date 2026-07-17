//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestSupplyObservedAt_StampsLedgerCloseTimeNotWallClock is the M4-callers
// end-to-end proof against a REAL ClickHouse lake. The bug: the supply-snapshot
// ledger resolvers (internal/ops/supply/supply.go::resolveSnapshotLedger and
// cmd/stellarindex-aggregator/main.go::supplyAggregatorLedgers.LatestKnownLedger)
// stamped a snapshot's ObservedAt with time.Now().UTC() instead of the chosen
// ledger's real close time — so a re-derived HISTORICAL supply snapshot carried
// the wall-clock write-time, corrupting point-in-time supply/observation
// queries (the operator re-derives supply constantly).
//
// The fix resolves the ledger's real close_time from stellar.ledgers via
// *clickhouse.ExplorerReader.CloseTimeForLedger and stamps THAT. This test
// seeds one stellar.ledgers row whose close_time is ~2.5y stale, resolves it
// through the production reader, feeds it through the production XLM computer
// exactly as the resolver→computer path does, and asserts the snapshot's
// ObservedAt equals the seeded close time — NOT ≈now. The pre-fix callers
// discarded this resolved value entirely (they never read the lake), so this
// stale, non-wall-clock ObservedAt is precisely what they could not produce.
func TestSupplyObservedAt_StampsLedgerCloseTimeNotWallClock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Isolated high ledger_seq + a deliberately stale close time (~2.5y before
	// this test runs) so a wall-clock stamp is unmistakable and no other test's
	// rows can collide.
	const ledger = uint32(210_000_007)
	closeTime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime, LedgerHash: "a1b2", PrevHash: "c3d4",
			ProtocolVersion: 22, BucketListHash: "e5f6",
			TxCount: 1, OpCount: 1, SorobanEventCount: 0,
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		},
	}
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	if err := sink.Add(ctx, ext); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	// 1. The production close-time read returns the ledger's REAL close time.
	got, found, err := reader.CloseTimeForLedger(ctx, ledger)
	if err != nil {
		t.Fatalf("CloseTimeForLedger: %v", err)
	}
	if !found {
		t.Fatalf("CloseTimeForLedger(%d) found=false — seeded ledger row not read back", ledger)
	}
	if !got.Equal(closeTime) {
		t.Fatalf("CloseTimeForLedger = %v, want the seeded close time %v", got, closeTime)
	}
	if time.Since(got) < 365*24*time.Hour {
		t.Fatalf("resolved close time %v is suspiciously close to now — reader returned wall-clock, not the lake close time", got)
	}

	// 2. The resolver→computer path stamps that close time onto the snapshot's
	//    ObservedAt (the XLM total is a constant, so a nil reserve reader is
	//    fine — this mirrors internal/supply/xlm_test.go's fixture).
	computer, err := supply.NewXLMComputer(nil, nil)
	if err != nil {
		t.Fatalf("NewXLMComputer: %v", err)
	}
	snap, err := computer.Compute(ctx, ledger, got)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if !snap.ObservedAt.Equal(closeTime) {
		t.Errorf("snapshot ObservedAt = %v, want the ledger close time %v (wall-clock stamp regression)", snap.ObservedAt, closeTime)
	}
	if time.Since(snap.ObservedAt) < 365*24*time.Hour {
		t.Errorf("snapshot ObservedAt %v is suspiciously close to now — the wall-clock M4-callers bug is back", snap.ObservedAt)
	}
}
