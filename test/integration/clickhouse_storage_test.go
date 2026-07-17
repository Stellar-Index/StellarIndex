//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
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

// TestClickHouseAccountMovementsRoundTrip is the ADR-0048 D2 write->read
// proof for stellar.account_movements: FanOutAccountMovement's direction
// rules (two-participant sent+received, self, single-participant "acting
// side") survive a real ClickHouse INSERT/SELECT round trip, a duplicate
// insert collapses under the table's ReplacingMergeTree engine (observed via
// VerifyAccountMovementsWindow's uniqExact, which — like the table's own
// dedup story — doesn't require FINAL to be correct), MaxAccountMovementLedger
// resolves the right resume point, and FindClaimableBalanceCreates'
// batched balance_id lookup (the ClickHouse replacement for the retired
// Postgres fallback) resolves a previously-written create.
func TestClickHouseAccountMovementsRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	if err := chstore.EnsureAccountMovementsTable(ctx, addr); err != nil {
		t.Fatalf("ensure account_movements table: %v", err)
	}

	const (
		ledger  = uint32(59_000_001) // pre-P23, arbitrary
		alice   = "GALICEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		bob     = "GBOBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		selfG   = "GSELFAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		creator = "GCREATORAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	)
	closeTime := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	balanceID := "deadbeef00112233"

	movements := []chstore.AccountMovement{
		{ // two-participant: payment Alice -> Bob
			MovementKind: "payment", Provenance: "classic_derived",
			Ledger: ledger, LedgerCloseTime: closeTime, TxHash: "tx_payment", OpIndex: 0,
			Asset: "native", Amount: big.NewInt(500), FromAddress: alice, ToAddress: bob,
		},
		{ // self-payment: must collapse to ONE 'self' row, not sent+received
			MovementKind: "payment", Provenance: "classic_derived",
			Ledger: ledger, LedgerCloseTime: closeTime, TxHash: "tx_self", OpIndex: 0,
			Asset: "native", Amount: big.NewInt(1), FromAddress: selfG, ToAddress: selfG,
		},
		{ // single-participant: claimable_balance_create (creator known, no claimant yet)
			MovementKind: "claimable_balance_create", Provenance: "classic_derived",
			Ledger: ledger, LedgerCloseTime: closeTime, TxHash: "tx_cb_create", OpIndex: 0,
			Asset: "native", Amount: big.NewInt(250), FromAddress: creator, ToAddress: "",
			Attributes: map[string]any{"balance_id": balanceID, "claimants": []string{bob}},
		},
	}

	// i128-scale amount that overflows int64, proving the Int128 column
	// round-trips without truncation (ADR-0003) even though classic amounts
	// in practice fit int64 — the column type is uniform across the lake.
	bigAmt, _ := new(big.Int).SetString("170141183460469231731687303715884105727", 10) // max int128
	movements = append(movements, chstore.AccountMovement{
		MovementKind: "payment", Provenance: "classic_derived",
		Ledger: ledger, LedgerCloseTime: closeTime, TxHash: "tx_i128", OpIndex: 0,
		Asset: "native", Amount: bigAmt, FromAddress: alice, ToAddress: bob,
	})

	written, err := chstore.InsertAccountMovements(ctx, addr, movements)
	if err != nil {
		t.Fatalf("InsertAccountMovements: %v", err)
	}
	// payment(2) + self(1) + claimable_balance_create(1) + i128 payment(2) = 6 rows.
	if written != 6 {
		t.Fatalf("written = %d, want 6 (post-fan-out row count)", written)
	}

	conn := dialClickHouse(t, ctx, "stellar")

	// ── Two-participant fan-out: sent + received rows, correct counterparty ──
	rows, err := conn.Query(ctx, `SELECT address, direction, counterparty, amount FROM stellar.account_movements WHERE tx_hash = 'tx_payment' ORDER BY direction`)
	if err != nil {
		t.Fatalf("query tx_payment: %v", err)
	}
	var gotRows int
	for rows.Next() {
		var gotAddr, gotDir, gotCounterparty string
		var gotAmt *big.Int
		if err := rows.Scan(&gotAddr, &gotDir, &gotCounterparty, &gotAmt); err != nil {
			t.Fatalf("scan tx_payment row: %v", err)
		}
		gotRows++
		switch gotDir {
		case "received":
			if gotAddr != bob || gotCounterparty != alice {
				t.Errorf("received row = address=%s counterparty=%s, want address=%s counterparty=%s", gotAddr, gotCounterparty, bob, alice)
			}
		case "sent":
			if gotAddr != alice || gotCounterparty != bob {
				t.Errorf("sent row = address=%s counterparty=%s, want address=%s counterparty=%s", gotAddr, gotCounterparty, alice, bob)
			}
		default:
			t.Errorf("unexpected direction %q", gotDir)
		}
		if gotAmt == nil || gotAmt.Cmp(big.NewInt(500)) != 0 {
			t.Errorf("amount = %v, want 500", gotAmt)
		}
	}
	_ = rows.Close()
	if gotRows != 2 {
		t.Fatalf("tx_payment produced %d rows, want 2", gotRows)
	}

	// ── Self-payment: exactly one 'self' row, no sent/received duplicate ──
	var selfCount uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.account_movements WHERE tx_hash = 'tx_self'`).Scan(&selfCount); err != nil {
		t.Fatalf("count tx_self: %v", err)
	}
	if selfCount != 1 {
		t.Fatalf("tx_self row count = %d, want 1", selfCount)
	}
	var selfDirection string
	if err := conn.QueryRow(ctx, `SELECT direction FROM stellar.account_movements WHERE tx_hash = 'tx_self'`).Scan(&selfDirection); err != nil {
		t.Fatalf("direction tx_self: %v", err)
	}
	if selfDirection != "self" {
		t.Fatalf("tx_self direction = %q, want self", selfDirection)
	}

	// ── i128 fidelity: the amount survives the round trip untruncated ──
	var i128Amt *big.Int
	if err := conn.QueryRow(ctx, `SELECT amount FROM stellar.account_movements WHERE tx_hash = 'tx_i128' AND direction = 'sent'`).Scan(&i128Amt); err != nil {
		t.Fatalf("query tx_i128: %v", err)
	}
	if i128Amt == nil || i128Amt.Cmp(bigAmt) != 0 {
		t.Fatalf("i128 amount = %v, want %v (must not truncate, ADR-0003)", i128Amt, bigAmt)
	}

	// ── Per-account ordered range read: the whole point of ORDER BY address ──
	var aliceCount uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.account_movements WHERE address = ?`, alice).Scan(&aliceCount); err != nil {
		t.Fatalf("count alice: %v", err)
	}
	if aliceCount != 2 { // tx_payment (sent) + tx_i128 (sent)
		t.Fatalf("alice row count = %d, want 2", aliceCount)
	}

	// ── MaxAccountMovementLedger: the data-derived resume point ──
	maxLedger, found, err := chstore.MaxAccountMovementLedger(ctx, addr, ledger, ledger)
	if err != nil {
		t.Fatalf("MaxAccountMovementLedger: %v", err)
	}
	if !found || maxLedger != ledger {
		t.Fatalf("MaxAccountMovementLedger = (%d, %v), want (%d, true)", maxLedger, found, ledger)
	}
	if _, found, err := chstore.MaxAccountMovementLedger(ctx, addr, ledger+1, ledger+1000); err != nil {
		t.Fatalf("MaxAccountMovementLedger (empty range): %v", err)
	} else if found {
		t.Fatalf("MaxAccountMovementLedger (empty range) found=true, want false")
	}

	// ── FindClaimableBalanceCreates: the ClickHouse Phase-3 batched fallback
	// lookup (2026-07-12: replaces a serial per-ref FindClaimableBalanceCreate
	// after the idx_cb_balance_id skip index made per-lookup cost negligible
	// but per-window lookup COUNT still mattered) — one query resolving a
	// found id, a missing id, and (via the empty-input short-circuit) the
	// no-op case together.
	foundCB, err := chstore.FindClaimableBalanceCreates(ctx, addr, []string{balanceID, "nonexistent"})
	if err != nil {
		t.Fatalf("FindClaimableBalanceCreates: %v", err)
	}
	row, ok := foundCB[balanceID]
	if !ok {
		t.Fatal("FindClaimableBalanceCreates: balanceID missing from result, want present")
	}
	if row.Asset != "native" || row.Amount == nil || row.Amount.Cmp(big.NewInt(250)) != 0 || row.CreatedBy != creator {
		t.Errorf("FindClaimableBalanceCreates[%s] = asset=%s amount=%v createdBy=%s, want native/250/%s", balanceID, row.Asset, row.Amount, row.CreatedBy, creator)
	}
	if _, ok := foundCB["nonexistent"]; ok {
		t.Fatal("FindClaimableBalanceCreates: \"nonexistent\" present in result, want absent")
	}
	if empty, err := chstore.FindClaimableBalanceCreates(ctx, addr, nil); err != nil {
		t.Fatalf("FindClaimableBalanceCreates (empty input): %v", err)
	} else if len(empty) != 0 {
		t.Fatalf("FindClaimableBalanceCreates (empty input) = %v, want empty map", empty)
	}

	// ── VerifyAccountMovementsWindow: uniqExact collapses the fan-out back
	// to per-movement counts (not per-row), matching what -verify compares
	// against the backfill command's own decode-time counts.
	verifyCounts, err := chstore.VerifyAccountMovementsWindow(ctx, addr, ledger, ledger)
	if err != nil {
		t.Fatalf("VerifyAccountMovementsWindow: %v", err)
	}
	if verifyCounts["payment"] != 3 { // tx_payment + tx_self + tx_i128
		t.Errorf("verifyCounts[payment] = %d, want 3", verifyCounts["payment"])
	}
	if verifyCounts["claimable_balance_create"] != 1 {
		t.Errorf("verifyCounts[claimable_balance_create] = %d, want 1", verifyCounts["claimable_balance_create"])
	}

	// ── Re-insert the IDENTICAL batch: ReplacingMergeTree dedup ──
	// (idempotent re-derivation, ADR-0048 D2's retry-safe write contract).
	// uniqExact is exact under un-merged duplicate parts (no FINAL needed —
	// see VerifyAccountMovementsWindow's doc comment), so the distinct
	// movement counts must be UNCHANGED by the duplicate insert even though
	// raw row counts may temporarily double until merges settle.
	if _, err := chstore.InsertAccountMovements(ctx, addr, movements); err != nil {
		t.Fatalf("InsertAccountMovements (duplicate): %v", err)
	}
	verifyCountsAfterDup, err := chstore.VerifyAccountMovementsWindow(ctx, addr, ledger, ledger)
	if err != nil {
		t.Fatalf("VerifyAccountMovementsWindow (after duplicate): %v", err)
	}
	if verifyCountsAfterDup["payment"] != 3 {
		t.Errorf("verifyCounts[payment] after duplicate insert = %d, want 3 (uniqExact must not double-count)", verifyCountsAfterDup["payment"])
	}
	if verifyCountsAfterDup["claimable_balance_create"] != 1 {
		t.Errorf("verifyCounts[claimable_balance_create] after duplicate insert = %d, want 1", verifyCountsAfterDup["claimable_balance_create"])
	}
}

// TestClickHouseProtocolDailyActivityDedup is a regression test for the
// contract_events_daily uniqExact→uniqCombined(17) redesign
// (docs/architecture/contract-events-daily-redesign.md, 2026-07-09
// incident): the whole point of using a uniq*-family aggregate (rather than
// a plain SummingMergeTree / countState()) is that a duplicate insert of the
// SAME natural key (ledger_seq, tx_hash, op_index, event_index) — a
// live-sink retry or a ch-rebuild re-derive re-inserting a range — does NOT
// inflate the count. This seeds 3 DISTINCT contract events plus a duplicate
// re-insert of one of them (identical natural key), then asserts both fast
// readers report exactly 3, not 4, proving uniqCombinedMerge(17) still
// dedups on the natural key rather than summing raw rows.
func TestClickHouseProtocolDailyActivityDedup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		baseLedger = uint32(72_000_001)
		contractID = "CTEST_DAILY_DEDUP_CCCCCCCCCCCCCCCCCCCCCCCCCCC"
	)
	closeTime := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)

	newEvent := func(ledgerOffset uint32, txHash string) chstore.ContractEventRow {
		return chstore.ContractEventRow{
			LedgerSeq:        baseLedger + ledgerOffset,
			CloseTime:        closeTime,
			TxHash:           txHash,
			OpIndex:          0,
			EventIndex:       0,
			ContractID:       contractID,
			EventType:        "contract",
			TopicCount:       1,
			Topic0Sym:        "swap",
			TopicsXDR:        []string{scval.MustEncodeSymbol("swap")},
			DataXDR:          scval.MustEncodeString("body"),
			OpArgsXDR:        []string{},
			InSuccessfulCall: 1,
		}
	}

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	// 3 distinct events (different ledger_seq/tx_hash each).
	evts := []chstore.ContractEventRow{
		newEvent(0, "dedup_tx_0000000000000000000000000000000000000000000000000000000000"),
		newEvent(1, "dedup_tx_1111111111111111111111111111111111111111111111111111111111"),
		newEvent(2, "dedup_tx_2222222222222222222222222222222222222222222222222222222222"),
	}
	for _, e := range evts {
		ext := chstore.LedgerExtract{
			Ledger: chstore.LedgerRow{LedgerSeq: e.LedgerSeq, CloseTime: closeTime, ProtocolVersion: 22, SorobanEventCount: 1},
			Events: []chstore.ContractEventRow{e},
		}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add: %v", err)
		}
	}
	// Duplicate re-insert of the FIRST event — identical natural key
	// (ledger_seq, tx_hash, op_index, event_index) — simulating a live-sink
	// retry. Must NOT be counted twice.
	dupExt := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{LedgerSeq: evts[0].LedgerSeq, CloseTime: closeTime, ProtocolVersion: 22, SorobanEventCount: 1},
		Events: []chstore.ContractEventRow{evts[0]},
	}
	if err := sink.Add(ctx, dupExt); err != nil {
		t.Fatalf("sink add (duplicate): %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	if !er.DailyActivityAvailable(ctx) {
		t.Fatal("DailyActivityAvailable = false after insert — contract_events_daily MV did not populate")
	}

	sinceDay := closeTime.AddDate(0, 0, -1)
	series, err := er.ProtocolDailyActivityFast(ctx, []string{contractID}, sinceDay)
	if err != nil {
		t.Fatalf("ProtocolDailyActivityFast: %v", err)
	}
	var total uint64
	for _, p := range series {
		total += p.Events
	}
	if total != 3 {
		t.Fatalf("ProtocolDailyActivityFast total = %d, want 3 — uniqCombinedMerge(17) counted the duplicate insert as a distinct event (dedup regression)", total)
	}

	breakdown, err := er.ProtocolEventBreakdownFast(ctx, []string{contractID}, sinceDay)
	if err != nil {
		t.Fatalf("ProtocolEventBreakdownFast: %v", err)
	}
	if got := breakdownCount(breakdown, "swap"); got != 3 {
		t.Fatalf("ProtocolEventBreakdownFast swap count = %d (rows=%v), want 3 — dedup regression", got, breakdown)
	}
}

// TestNetworkThroughput_DedupsReingestedLedger is the audit C2-12 guard:
// stellar.ledgers is ReplacingMergeTree, and a re-ingested ledger (a ch-backfill
// re-derive) leaves an un-merged duplicate PART until a background merge. Before
// the fix NetworkThroughput's count()/sum(*_count) ran WITHOUT FINAL, so during
// that window the served daily throughput double-counted the ledger and doubled
// its tx/op/event sums. The fix reads FROM stellar.ledgers FINAL. This inserts a
// ledger twice (two parts, identical RMT identity) and asserts the served bucket
// counts it ONCE. Uses a very high, isolated ledger_seq + a unique close date so
// the windowed aggregate can't be contaminated by other tests' rows.
func TestNetworkThroughput_DedupsReingestedLedger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const ledger = uint32(200_000_000) // above any other test's ledgers → the max
	closeTime := time.Date(2027, 6, 15, 9, 0, 0, 0, time.UTC)
	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime, LedgerHash: "dd00", PrevHash: "ee00",
			ProtocolVersion: 22, BucketListHash: "ff00",
			TxCount: 7, OpCount: 3, SorobanEventCount: 2,
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		},
	}
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	// Ingest, then RE-INGEST the identical ledger → two ReplacingMergeTree parts
	// sharing one ORDER-BY identity (ledger_seq), un-merged at read time.
	for i := 0; i < 2; i++ {
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add #%d: %v", i, err)
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("sink flush #%d: %v", i, err)
		}
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	buckets, err := er.NetworkThroughput(ctx, 1) // 1-day window → only the tip region (this ledger)
	if err != nil {
		t.Fatalf("NetworkThroughput: %v", err)
	}
	day := closeTime.Truncate(24 * time.Hour)
	var found bool
	for _, b := range buckets {
		if !b.Day.Equal(day) {
			continue
		}
		found = true
		if b.Ledgers != 1 {
			t.Errorf("Ledgers = %d, want 1 (a re-ingested ledger must count once — un-merged RMT part double-counted)", b.Ledgers)
		}
		if b.Txs != 7 || b.Ops != 3 || b.Events != 2 {
			t.Errorf("sums = (tx=%d op=%d ev=%d), want (7,3,2) — FINAL missing lets the duplicate part double the sums", b.Txs, b.Ops, b.Events)
		}
	}
	if !found {
		t.Fatalf("no throughput bucket for %v — the test ledger fell outside the window (another test inserted a higher ledger_seq?)", day)
	}
}
