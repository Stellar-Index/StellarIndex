//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestEventCensusShortfalls_DroppedEventPartition is the #806 proof on a real
// ClickHouse: a DROP PARTITION on stellar.contract_events leaves stellar.ledgers
// contiguous and hash-chained, so SubstrateProblem still reports the range
// intact — the census is the only reader that sees the events are gone.
func TestEventCensusShortfalls_DroppedEventPartition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Two isolated partitions nothing else in the suite writes: 150 and 151 (below every range another CH test treats as the global max).
	const p0, p1 = uint32(150_000_000), uint32(151_000_000)
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	seed := func(seq, events uint32) {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: time.Date(2027, 8, 1, 0, 0, 0, 0, time.UTC),
			LedgerHash: fmt.Sprintf("h%d", seq), PrevHash: fmt.Sprintf("h%d", seq-1),
			ProtocolVersion: 22, BucketListHash: "cc00",
			SorobanEventCount: events,
			TotalCoins:        1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		for i := uint32(0); i < events; i++ {
			ext.Events = append(ext.Events, chstore.ContractEventRow{
				LedgerSeq: seq, CloseTime: ext.Ledger.CloseTime, TxHash: fmt.Sprintf("tx%d", seq),
				EventIndex: i, ContractID: "CCENSUSFIXTURE", EventType: "contract",
				TopicCount: 1, Topic0Sym: "transfer", InSuccessfulCall: 1,
			})
		}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}
	// A hash-chained run straddling the two partitions, 2 events per ledger.
	for seq := p1 - 3; seq <= p1+2; seq++ {
		seed(seq, 2)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	short, err := chstore.EventCensusShortfalls(ctx, addr, p0, p1+2)
	if err != nil {
		t.Fatalf("EventCensusShortfalls (intact): %v", err)
	}
	if len(short) != 0 {
		t.Fatalf("intact lake reported shortfalls %+v, want none", short)
	}

	conn := dialClickHouse(t, ctx, "stellar")
	if err := conn.Exec(ctx, "ALTER TABLE stellar.contract_events DROP PARTITION 151"); err != nil {
		t.Fatalf("drop partition: %v", err)
	}

	// The defect: the substrate axis cannot see it.
	if p, has, d, err := chstore.SubstrateProblem(ctx, addr, p1-3, p1+2); err != nil || has {
		t.Fatalf("SubstrateProblem after the event drop = (%d, %v, %q, %v); this test assumes ledgers stay intact", p, has, d, err)
	}

	short, err = chstore.EventCensusShortfalls(ctx, addr, p0, p1+2)
	if err != nil {
		t.Fatalf("EventCensusShortfalls (dropped): %v", err)
	}
	if len(short) != 1 {
		t.Fatalf("shortfalls = %+v, want exactly partition 151", short)
	}
	got := short[0]
	want := chstore.EventCensusShortfall{Partition: 151, FirstEventLedger: p1, Expected: 6, Present: 0}
	if got != want {
		t.Fatalf("shortfall = %+v, want %+v", got, want)
	}
}
