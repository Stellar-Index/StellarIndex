//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestClickHouseContractEventsRMTDedup is the live-ClickHouse proof for audit
// W4-storage-1: ContractEventsRecent (Go-side adjacent-row dedup since
// 2026-08-06 — the SQL LIMIT 1 BY disabled reverse read-in-order and cost
// 100× on busy contracts) and EventsByTx (FINAL) must serve each contract
// event EXACTLY ONCE even while
// stellar.contract_events — a ReplacingMergeTree — holds an un-merged duplicate
// part (the legitimate post-heal / ch-rebuild / partial-flush-retry state).
//
// Determinism: the two same-key inserts create two parts, and a background merge
// would collapse them on its own (making the pre-fix reader accidentally pass).
// SYSTEM STOP MERGES pins the table in its un-merged state for the duration, so
// the dedup MUST come from the query — reverting the fix makes both readers
// return 4 rows (2 events x 2 parts) instead of 2.
func TestClickHouseContractEventsRMTDedup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		ledger     = uint32(70_100_001)
		contractID = "CTEST_W4_RMT_DEDUP_AAAAAAAAAAAAAAAAAAAAAAAAAAA"
		txHash     = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	closeTime := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

	// Pin the table un-merged so the two parts genuinely coexist at read time.
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, "SYSTEM STOP MERGES stellar.contract_events"); err != nil {
		t.Fatalf("SYSTEM STOP MERGES: %v", err)
	}
	t.Cleanup(func() {
		_ = raw.Exec(context.Background(), "SYSTEM START MERGES stellar.contract_events")
	})

	// Two DISTINCT events in one tx — proves dedup collapses duplicate PARTS
	// without also collapsing distinct events (event_index 0 vs 1).
	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime, LedgerHash: "dd00dd00", PrevHash: "ee00ee00",
			ProtocolVersion: 22, TxCount: 1, OpCount: 1, SorobanEventCount: 2,
		},
		Events: []chstore.ContractEventRow{
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHash, OpIndex: 0, EventIndex: 0,
				ContractID: contractID, EventType: "contract", TopicCount: 1, Topic0Sym: "mint",
				TopicsXDR: []string{scval.MustEncodeSymbol("mint")}, DataXDR: scval.MustEncodeString("a"),
				OpArgsXDR: []string{}, InSuccessfulCall: 1,
			},
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHash, OpIndex: 0, EventIndex: 1,
				ContractID: contractID, EventType: "contract", TopicCount: 1, Topic0Sym: "transfer",
				TopicsXDR: []string{scval.MustEncodeSymbol("transfer")}, DataXDR: scval.MustEncodeString("b"),
				OpArgsXDR: []string{}, InSuccessfulCall: 1,
			},
		},
	}

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	// Two separate flushes of the IDENTICAL extract → two un-merged parts, each
	// carrying the same two primary-key rows (the RMT idempotent-re-ingest state).
	for i := 0; i < 2; i++ {
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add (pass %d): %v", i, err)
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("sink flush (pass %d): %v", i, err)
		}
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	// ── ContractEventsRecent: LIMIT 1 BY primary key ────────────────────────
	recent, err := er.ContractEventsRecent(ctx, contractID, 100, chstore.ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("ContractEventsRecent returned %d rows, want 2 — the LIMIT 1 BY dedup must collapse "+
			"the duplicate un-merged part (pre-fix: 4)", len(recent))
	}
	// Distinct events preserved: exactly one row per event_index (0 and 1).
	seen := map[uint32]int{}
	for _, e := range recent {
		seen[e.EventIndex]++
	}
	if seen[0] != 1 || seen[1] != 1 {
		t.Fatalf("ContractEventsRecent event_index multiplicity = %v, want each exactly once", seen)
	}

	// ── EventsByTx: FINAL ───────────────────────────────────────────────────
	byTx, err := er.EventsByTx(ctx, ledger, txHash)
	if err != nil {
		t.Fatalf("EventsByTx: %v", err)
	}
	if len(byTx) != 2 {
		t.Fatalf("EventsByTx returned %d rows, want 2 — FINAL must collapse the duplicate un-merged "+
			"part (pre-fix: 4)", len(byTx))
	}
	seenTx := map[uint32]int{}
	for _, e := range byTx {
		seenTx[e.EventIndex]++
	}
	if seenTx[0] != 1 || seenTx[1] != 1 {
		t.Fatalf("EventsByTx event_index multiplicity = %v, want each exactly once", seenTx)
	}
}
