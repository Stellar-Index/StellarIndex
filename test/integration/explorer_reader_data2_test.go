//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestClickHouseContractActivitySummaryRMTDedup is the live-ClickHouse proof
// for audit CHQ-2: ContractActivitySummaryFor read stellar.contract_active_ledgers
// (a ReplacingMergeTree) with a bare count(), so an overlapping backfill window
// that re-inserts the same (contract, ledger) keys as a second un-merged part
// inflated ActiveLedgersTotal (a headline card number) and the daily bars up to
// ~2x until a background merge. The fix counts uniqExact(ledger_seq). SYSTEM STOP
// MERGES pins the two parts un-merged so the dedup MUST come from the query —
// reverting the fix makes the total read 6 instead of the 3 distinct ledgers.
func TestClickHouseContractActivitySummaryRMTDedup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const contractID = "CTEST_CHQ2_ACTIVITY_DEDUP_AAAAAAAAAAAAAAAAAAA"
	// Three DISTINCT active ledgers, all recent so they land inside the daily
	// window (close_time within the last few days).
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Hour)
	ledgers := []uint32{80_100_001, 80_100_002, 80_100_003}

	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, "SYSTEM STOP MERGES stellar.contract_active_ledgers"); err != nil {
		t.Fatalf("SYSTEM STOP MERGES: %v", err)
	}
	t.Cleanup(func() {
		_ = raw.Exec(context.Background(), "SYSTEM START MERGES stellar.contract_active_ledgers")
	})

	// Two IDENTICAL inserts → two un-merged parts, each carrying the same three
	// (contract, ledger) keys (the overlapping-backfill / re-ingest state).
	for pass := 0; pass < 2; pass++ {
		b, err := raw.PrepareBatch(ctx,
			`INSERT INTO stellar.contract_active_ledgers (contract_id, ledger_seq, close_time, ingested_at)`)
		if err != nil {
			t.Fatalf("prepare active_ledgers batch (pass %d): %v", pass, err)
		}
		ing := time.Now().UTC().Add(time.Duration(pass) * time.Minute)
		for i, l := range ledgers {
			if err := b.Append(contractID, l, base.Add(time.Duration(i)*time.Hour), ing); err != nil {
				t.Fatalf("append active ledger (pass %d): %v", pass, err)
			}
		}
		if err := b.Send(); err != nil {
			t.Fatalf("send active_ledgers batch (pass %d): %v", pass, err)
		}
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	s, ok, err := er.ContractActivitySummaryFor(ctx, contractID, 30)
	if err != nil || !ok {
		t.Fatalf("ContractActivitySummaryFor: ok=%v err=%v", ok, err)
	}
	if s.ActiveLedgersTotal != 3 {
		t.Fatalf("ActiveLedgersTotal = %d, want 3 distinct ledgers — the un-merged duplicate part "+
			"must be deduped by uniqExact (pre-fix count(): 6)", s.ActiveLedgersTotal)
	}
	var dailySum uint64
	for _, d := range s.Daily {
		dailySum += d.ActiveLedgers
	}
	if dailySum != 3 {
		t.Fatalf("daily active-ledger sum = %d across %d days, want 3 (per-day bars must dedup too)",
			dailySum, len(s.Daily))
	}
}

// TestClickHouseContractEventsRecentPartialBackfill is the live-ClickHouse proof
// for audit W1-chrollup-3: contract_active_ledgers' availability probe is a
// LIMIT-1 table-global emptiness check that cannot see PARTIAL backfill
// coverage. In the applied-but-still-backfilling state the index is globally
// non-empty (some OTHER contract's rows) but holds NO rows for a quiet contract
// whose events do exist in contract_events. The old reader trusted that empty
// per-contract walk and served an authoritative "no events". The fix falls
// through to the unbounded contract_events scan (the source of truth). Reverting
// the fix makes ContractEventsRecent return 0 rows here instead of the real event.
func TestClickHouseContractEventsRecentPartialBackfill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		quietContract = "CTEST_CHROLLUP3_QUIET_AAAAAAAAAAAAAAAAAAAAAAA"
		decoyContract = "CTEST_CHROLLUP3_DECOY_AAAAAAAAAAAAAAAAAAAAAAA"
		ledger        = uint32(5_000_101) // low ledger: the un-backfilled prefix
		txHash        = "3333333333333333333333333333333333333333333333333333333333333333"
	)
	closeTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	// Ingest the quiet contract's event. This also populates
	// contract_active_ledgers via its MV — which we then TRUNCATE to
	// reproduce the partial-backfill state (index present, this contract's
	// coverage NOT yet backfilled).
	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime, LedgerHash: "aa11aa11", PrevHash: "bb22bb22",
			ProtocolVersion: 22, TxCount: 1, OpCount: 1, SorobanEventCount: 1,
		},
		Events: []chstore.ContractEventRow{{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHash, OpIndex: 0, EventIndex: 0,
			ContractID: quietContract, EventType: "contract", TopicCount: 1, Topic0Sym: "transfer",
			TopicsXDR: []string{scval.MustEncodeSymbol("transfer")}, DataXDR: scval.MustEncodeString("x"),
			OpArgsXDR: []string{}, InSuccessfulCall: 1,
		}},
	}
	if err := sink.Add(ctx, ext); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	raw := dialClickHouse(t, ctx, "stellar")
	// Wipe the MV-populated coverage, then seed ONE decoy row so the table is
	// globally non-empty (probe passes) but holds nothing for the quiet
	// contract — the exact partial-backfill state the finding describes.
	if err := raw.Exec(ctx, "TRUNCATE TABLE stellar.contract_active_ledgers"); err != nil {
		t.Fatalf("truncate active_ledgers: %v", err)
	}
	db, err := raw.PrepareBatch(ctx,
		`INSERT INTO stellar.contract_active_ledgers (contract_id, ledger_seq, close_time, ingested_at)`)
	if err != nil {
		t.Fatalf("prepare decoy batch: %v", err)
	}
	if err := db.Append(decoyContract, uint32(90_000_000), closeTime, time.Now().UTC()); err != nil {
		t.Fatalf("append decoy: %v", err)
	}
	if err := db.Send(); err != nil {
		t.Fatalf("send decoy: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	rows, err := er.ContractEventsRecent(ctx, quietContract, 100, chstore.ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ContractEventsRecent returned %d rows, want 1 — the partial-backfill empty walk must "+
			"fall through to the unbounded contract_events scan, not serve a confidently-wrong empty page "+
			"(pre-fix: 0)", len(rows))
	}
	if rows[0].Seq != ledger {
		t.Fatalf("served event at ledger %d, want %d", rows[0].Seq, ledger)
	}
}
