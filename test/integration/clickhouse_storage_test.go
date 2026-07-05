//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/events"
	"github.com/StellarIndex/stellar-index/internal/scval"
	chstore "github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
)

// TestClickHouseLakeRoundTrip is the first end-to-end proof of the Tier-1 lake
// (ADR-0034) Go write→read path against a real ClickHouse: it writes a ledger,
// a contract event, and two supply-flow events through the repo's own Sink and
// reads them back through the repo's own ExplorerReader / SupplyReader /
// StreamContractEvents — asserting field fidelity, i128 amount fidelity (values
// that overflow int64, kept as *big.Int / Int128, never truncated — ADR-0003),
// and ReplacingMergeTree dedup (a duplicate insert collapses on a FINAL read).
func TestClickHouseLakeRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		baseLedger = uint32(70_000_001)
		contractID = "CTEST_ROUNDTRIP_TOKEN_AAAAAAAAAAAAAAAAAAAAAA"
		txHash     = "1111111111111111111111111111111111111111111111111111111111111111"
	)
	closeTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	// i128 magnitudes that OVERFLOW int64 (max ≈ 9.22e18), to prove the amount
	// survives write→store→read as a *big.Int / Int128 and is never truncated.
	mintAmt, _ := new(big.Int).SetString("1000000000000000000000000000000", 10) // 1e30
	burnAmt, _ := new(big.Int).SetString("250000000000000000000", 10)           // 2.5e20

	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq:         baseLedger,
			CloseTime:         closeTime,
			LedgerHash:        "aa00aa00",
			PrevHash:          "bb00bb00",
			ProtocolVersion:   22,
			BucketListHash:    "cc00cc00",
			TxCount:           1,
			OpCount:           1,
			SorobanEventCount: 1,
			TotalCoins:        1_050_000_000_123_456_789, // XLM stroops, > 2^53
			FeePool:           987_654,
			BaseFee:           100,
			BaseReserve:       5_000_000,
		},
		Events: []chstore.ContractEventRow{{
			LedgerSeq:        baseLedger,
			CloseTime:        closeTime,
			TxHash:           txHash,
			OpIndex:          0,
			EventIndex:       0,
			ContractID:       contractID,
			EventType:        "contract",
			TopicCount:       2,
			Topic0Sym:        "mint",
			TopicsXDR:        []string{scval.MustEncodeSymbol("mint"), scval.MustEncodeString("dest")},
			DataXDR:          scval.MustEncodeString("payload"),
			OpArgsXDR:        []string{},
			InSuccessfulCall: 1,
		}},
		SupplyFlows: []chstore.SupplyFlowRow{
			{ContractID: contractID, LedgerSeq: baseLedger, CloseTime: closeTime, TxHash: txHash, OpIndex: 0, EventIndex: 0, Kind: "mint", Amount: mintAmt},
			{ContractID: contractID, LedgerSeq: baseLedger, CloseTime: closeTime, TxHash: txHash, OpIndex: 0, EventIndex: 1, Kind: "burn", Amount: burnAmt},
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
	// RMT dedup: re-insert the IDENTICAL extract. Every row's ReplacingMergeTree
	// ORDER-BY identity is unchanged, so a FINAL read must collapse the
	// duplicates — the lake's idempotent-re-ingest guarantee (ADR-0034: "NO ON
	// CONFLICT silent-drop like the Postgres soroban_events bug").
	if err := sink.Add(ctx, ext); err != nil {
		t.Fatalf("sink add (duplicate): %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush (duplicate): %v", err)
	}

	// ── Ledger header round-trip (ExplorerReader.LedgerBySeq, FINAL) ─────────
	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	lh, found, err := er.LedgerBySeq(ctx, baseLedger)
	if err != nil || !found {
		t.Fatalf("LedgerBySeq(%d): found=%v err=%v", baseLedger, found, err)
	}
	if lh.TotalCoins != ext.Ledger.TotalCoins {
		t.Errorf("ledger TotalCoins = %d, want %d (i64 > 2^53 fidelity)", lh.TotalCoins, ext.Ledger.TotalCoins)
	}
	if lh.SorobanEventCount != 1 || lh.TxCount != 1 || lh.ProtocolVersion != 22 {
		t.Errorf("ledger header mismatch: got soroban=%d tx=%d proto=%d", lh.SorobanEventCount, lh.TxCount, lh.ProtocolVersion)
	}

	// ── Contract-event round-trip (StreamContractEvents, FINAL) ──────────────
	var gotEvents []events.Event
	if err := chstore.StreamContractEvents(ctx, addr, baseLedger, baseLedger, nil, func(e events.Event) error {
		gotEvents = append(gotEvents, e)
		return nil
	}); err != nil {
		t.Fatalf("StreamContractEvents: %v", err)
	}
	if len(gotEvents) != 1 {
		t.Fatalf("StreamContractEvents returned %d events, want 1 (duplicate must collapse under FINAL)", len(gotEvents))
	}
	ev := gotEvents[0]
	if ev.ContractID != contractID || ev.TxHash != txHash || ev.Type != "contract" {
		t.Errorf("event identity mismatch: got contract=%s tx=%s type=%s", ev.ContractID, ev.TxHash, ev.Type)
	}
	if len(ev.Topic) != 2 || ev.Topic[0] != scval.MustEncodeSymbol("mint") || ev.Topic[1] != scval.MustEncodeString("dest") {
		t.Errorf("event topics not preserved byte-for-byte: %v", ev.Topic)
	}
	if ev.Value != scval.MustEncodeString("payload") {
		t.Errorf("event value not preserved: got %q", ev.Value)
	}

	// ── Supply round-trip: i128 fidelity + RMT dedup (SupplyReader, FINAL) ───
	sr, err := chstore.NewSupplyReader(ctx, addr)
	if err != nil {
		t.Fatalf("new supply reader: %v", err)
	}
	t.Cleanup(func() { _ = sr.Close() })

	ts, err := sr.TokenSupply(ctx, contractID)
	if err != nil {
		t.Fatalf("TokenSupply: %v", err)
	}
	// Two DISTINCT flow identities (mint@event0, burn@event1); the duplicate
	// insert of each collapses under FINAL → exactly 2 flows, not 4.
	if ts.FlowCount != 2 {
		t.Fatalf("FlowCount = %d, want 2 — ReplacingMergeTree FINAL did not collapse the duplicate inserts", ts.FlowCount)
	}
	if ts.Mint.Cmp(mintAmt) != 0 {
		t.Errorf("Mint = %s, want %s — i128 amount truncated/corrupted", ts.Mint, mintAmt)
	}
	if ts.Burn.Cmp(burnAmt) != 0 {
		t.Errorf("Burn = %s, want %s", ts.Burn, burnAmt)
	}
	wantTotal := new(big.Int).Sub(mintAmt, burnAmt)
	if ts.Total.Cmp(wantTotal) != 0 {
		t.Errorf("Total = %s, want %s (Σmint − Σburn)", ts.Total, wantTotal)
	}
}

// TestClickHouseTxHashIndexProbeFallback exercises ExplorerReader.TransactionByHash's
// two-mode resolution (perf-todo §4): the hash-ordered stellar.tx_hash_index
// fast path, and the tx_hash bloom-scan FALLBACK taken on an index miss. To make
// the branch OBSERVABLE, two transactions share one hash at different ledgers
// with a controlled ingested_at ordering:
//   - the index maps hash → ledger A, so the fast path resolves to A;
//   - a bloom SCAN (no ledger scope, latest-ingested wins) resolves to ledger B.
//
// So a returned Seq of A proves the index was used, and B proves the fallback
// scan was used. Fixtures are seeded via a raw connection to control ingested_at
// + the index rows precisely; the behaviour under test runs through the real
// ExplorerReader. (The lake tables are shared per-binary but the integration
// tests run sequentially and no other test touches tx_hash_index, so the
// TRUNCATEs here are safe.)
func TestClickHouseTxHashIndexProbeFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const (
		ledgerA = uint32(72_000_001) // index target + fast-path answer
		ledgerB = uint32(72_000_500) // latest-ingested + scan answer
		txHash  = "2222222222222222222222222222222222222222222222222222222222222222"
		txIdxA  = uint32(3)
		txIdxB  = uint32(9)
	)
	closeTime := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	// Strictly increasing ingested_at so the bloom scan's `ORDER BY ingested_at
	// DESC LIMIT 1` deterministically prefers the ledger-B row.
	ingA := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	ingB := ingA.Add(time.Hour)

	txBatch, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.transactions
		(ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee,
		 operation_count, successful, result_code, memo_type, memo, ingested_at)`)
	if err != nil {
		t.Fatalf("prepare transactions batch: %v", err)
	}
	if err := txBatch.Append(ledgerA, closeTime, txHash, txIdxA, "GSRC", int64(100), int64(200), uint16(1), uint8(1), int32(0), "none", "at-ledger-A", ingA); err != nil {
		t.Fatalf("append tx A: %v", err)
	}
	if err := txBatch.Append(ledgerB, closeTime, txHash, txIdxB, "GSRC", int64(100), int64(200), uint16(1), uint8(1), int32(0), "none", "at-ledger-B", ingB); err != nil {
		t.Fatalf("append tx B: %v", err)
	}
	if err := txBatch.Send(); err != nil {
		t.Fatalf("send transactions batch: %v", err)
	}

	// The tx_hash_index_mv auto-indexed BOTH rows on the insert above; reset the
	// index and seed exactly one mapping hash → ledger A so the fast path has a
	// single, known answer distinct from the scan's.
	seedIndex := func() {
		if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.tx_hash_index`); err != nil {
			t.Fatalf("truncate tx_hash_index: %v", err)
		}
		ib, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.tx_hash_index (tx_hash, ledger_seq, tx_index)`)
		if err != nil {
			t.Fatalf("prepare tx_hash_index batch: %v", err)
		}
		if err := ib.Append(txHash, ledgerA, txIdxA); err != nil {
			t.Fatalf("append index row: %v", err)
		}
		if err := ib.Send(); err != nil {
			t.Fatalf("send tx_hash_index batch: %v", err)
		}
	}

	// ── Fast path: index present (hash → ledger A) ───────────────────────────
	seedIndex()
	er1, err := chstore.NewExplorerReader(ctx, addr) // fresh reader = fresh probe-once
	if err != nil {
		t.Fatalf("new explorer reader (hit): %v", err)
	}
	t.Cleanup(func() { _ = er1.Close() })
	txHit, found, err := er1.TransactionByHash(ctx, txHash)
	if err != nil || !found {
		t.Fatalf("TransactionByHash (index hit): found=%v err=%v", found, err)
	}
	if txHit.Seq != ledgerA || txHit.TxIndex != txIdxA {
		t.Fatalf("index hit resolved to ledger %d (tx_index %d), want %d/%d — fast path (tx_hash_index) not used",
			txHit.Seq, txHit.TxIndex, ledgerA, txIdxA)
	}

	// ── Fallback: index empty → bloom scan (latest-ingested = ledger B) ──────
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.tx_hash_index`); err != nil {
		t.Fatalf("truncate tx_hash_index (fallback): %v", err)
	}
	er2, err := chstore.NewExplorerReader(ctx, addr) // fresh probe; table still EXISTS (empty) → fast path attempted then misses
	if err != nil {
		t.Fatalf("new explorer reader (fallback): %v", err)
	}
	t.Cleanup(func() { _ = er2.Close() })
	txScan, found, err := er2.TransactionByHash(ctx, txHash)
	if err != nil || !found {
		t.Fatalf("TransactionByHash (fallback scan): found=%v err=%v", found, err)
	}
	if txScan.Seq != ledgerB || txScan.TxIndex != txIdxB {
		t.Fatalf("fallback resolved to ledger %d (tx_index %d), want %d/%d — bloom-scan fallback not used",
			txScan.Seq, txScan.TxIndex, ledgerB, txIdxB)
	}
}

// TestClickHouseProtocolBreakdownT0XDR exercises the protocol event-breakdown
// reader's fast path (stellar.contract_events_daily) vs its raw-scan path
// (stellar.contract_events) around the t0_xdr column (BACKLOG #55 / #43). The
// seeded event is Phoenix-shaped: topic[0] is a non-Symbol String action name
// ("swap"), so topic_0_sym is EMPTY and the label can only be recovered from
// the raw topic[0] XDR (t0_xdr) — while topic[1] is a String FIELD name
// ("sender") that must NOT be mistaken for the action. Both readers must label
// the event "swap".
func TestClickHouseProtocolBreakdownT0XDR(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		baseLedger = uint32(71_000_001)
		contractID = "CTEST_PHOENIX_POOL_BBBBBBBBBBBBBBBBBBBBBBBBBB"
		txHash     = "3333333333333333333333333333333333333333333333333333333333333333"
	)
	closeTime := time.Date(2026, 4, 10, 8, 0, 0, 0, time.UTC)

	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{LedgerSeq: baseLedger, CloseTime: closeTime, ProtocolVersion: 22, SorobanEventCount: 1},
		Events: []chstore.ContractEventRow{{
			LedgerSeq:  baseLedger,
			CloseTime:  closeTime,
			TxHash:     txHash,
			OpIndex:    0,
			EventIndex: 0,
			ContractID: contractID,
			EventType:  "contract",
			TopicCount: 2,
			Topic0Sym:  "", // non-Symbol topic[0] → empty denormalized symbol
			// topics_xdr[1] = topic[0] = String("swap") → the daily MV captures it as t0_xdr;
			// topics_xdr[2] = topic[1] = String("sender") → captured as t1_xdr.
			TopicsXDR:        []string{scval.MustEncodeString("swap"), scval.MustEncodeString("sender")},
			DataXDR:          scval.MustEncodeString("body"),
			OpArgsXDR:        []string{},
			InSuccessfulCall: 1,
		}},
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

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	// The contract_events_daily materialized view populates synchronously on the
	// insert above, so the fast path is available.
	if !er.DailyActivityAvailable(ctx) {
		t.Fatal("DailyActivityAvailable = false after insert — contract_events_daily MV did not populate")
	}

	// Raw-scan path (stellar.contract_events): recovers "swap" from topic[0] XDR.
	raw, err := er.ProtocolEventBreakdown(ctx, []string{contractID}, 0)
	if err != nil {
		t.Fatalf("ProtocolEventBreakdown (raw scan): %v", err)
	}
	if got := breakdownCount(raw, "swap"); got != 1 {
		t.Fatalf("raw-scan breakdown swap count = %d (rows=%v), want 1 — t0_xdr recovery failed on the raw path", got, raw)
	}

	// Fast path (stellar.contract_events_daily.t0_xdr): must recover the SAME label.
	sinceDay := closeTime.AddDate(0, 0, -1)
	fast, err := er.ProtocolEventBreakdownFast(ctx, []string{contractID}, sinceDay)
	if err != nil {
		t.Fatalf("ProtocolEventBreakdownFast (daily preagg): %v", err)
	}
	if got := breakdownCount(fast, "swap"); got != 1 {
		t.Fatalf("fast-path breakdown swap count = %d (rows=%v), want 1 — t0_xdr recovery failed on the daily preagg", got, fast)
	}
}

// breakdownCount returns the event count for a given effective event name in a
// protocol breakdown result (0 if absent).
func breakdownCount(rows []chstore.ProtocolEventTypeCount, name string) uint64 {
	for _, r := range rows {
		if r.EventType == name {
			return r.Count
		}
	}
	return 0
}
