//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
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

	if err := sink.Add(ctx, withEventTxs(ext)); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}
	// RMT dedup: re-insert the IDENTICAL extract. Every row's ReplacingMergeTree
	// ORDER-BY identity is unchanged, so a FINAL read must collapse the
	// duplicates — the lake's idempotent-re-ingest guarantee (ADR-0034: "NO ON
	// CONFLICT silent-drop like the Postgres soroban_events bug").
	if err := sink.Add(ctx, withEventTxs(ext)); err != nil {
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
// fast path, and the tx_hash bloom-scan FALLBACK taken when the index EXISTS
// but is EMPTY — the MV-drop / TRUNCATE pathology, in which the availability
// probe must treat the index as unavailable rather than let its emptiness
// mint authoritative 404s for every real hash (a per-hash miss against a
// NON-EMPTY index stays authoritative; the unit tests in
// internal/storage/clickhouse/tx_hash_index_test.go pin that arm). To make
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
		if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.tx_hash_index_coverage`); err != nil {
			t.Fatalf("truncate tx_hash_index_coverage: %v", err)
		}
		if err := conn.Exec(ctx, `INSERT INTO stellar.tx_hash_index_coverage (covered_from, covered_to) VALUES (2, 72000500)`); err != nil {
			t.Fatalf("insert coverage marker: %v", err)
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

	// ── Fallback: index seeded but no coverage marker → scan ─────────────────
	seedIndex()
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.tx_hash_index_coverage`); err != nil {
		t.Fatalf("truncate tx_hash_index_coverage: %v", err)
	}
	er3, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader (no marker): %v", err)
	}
	t.Cleanup(func() { _ = er3.Close() })
	txNoMarker, found, err := er3.TransactionByHash(ctx, txHash)
	if err != nil || !found {
		t.Fatalf("TransactionByHash (no marker): found=%v err=%v", found, err)
	}
	if txNoMarker.Seq != ledgerB {
		t.Fatalf("no-marker lookup resolved to ledger %d, want %d — index used without a coverage marker", txNoMarker.Seq, ledgerB)
	}

	// ── Fallback: index empty → bloom scan (latest-ingested = ledger B) ──────
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.tx_hash_index`); err != nil {
		t.Fatalf("truncate tx_hash_index (fallback): %v", err)
	}
	er2, err := chstore.NewExplorerReader(ctx, addr) // fresh probe; table EXISTS but is EMPTY → index treated as unavailable, scan path
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
	// insert above, so the fast path is available — and the answer is
	// DEFINITIVE (rows found), so callers may cache it.
	if avail, definitive := er.DailyActivityAvailable(ctx); !avail || !definitive {
		t.Fatalf("DailyActivityAvailable = (%v,%v) after insert, want (true,true) — contract_events_daily MV did not populate", avail, definitive)
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
// Postgres fallback) resolves a already-written create.
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

	// ── MinAccountMovementLedger: -resume's contiguity check (Q216) ──
	minLedger, minFound, err := chstore.MinAccountMovementLedger(ctx, addr, ledger, ledger)
	if err != nil {
		t.Fatalf("MinAccountMovementLedger: %v", err)
	}
	if !minFound || minLedger != ledger {
		t.Fatalf("MinAccountMovementLedger = (%d, %v), want (%d, true)", minLedger, minFound, ledger)
	}
	if _, found, err := chstore.MinAccountMovementLedger(ctx, addr, ledger+1, ledger+1000); err != nil {
		t.Fatalf("MinAccountMovementLedger (empty range): %v", err)
	} else if found {
		t.Fatalf("MinAccountMovementLedger (empty range) found=true, want false")
	}

	// ── FindClaimableBalanceCreates: the ClickHouse Phase-3 batched fallback
	// lookup (one query per window rather than a serial per-ref
	// FindClaimableBalanceCreate; the idx_cb_balance_id skip index makes
	// per-lookup cost negligible but per-window lookup COUNT still matters) — one query resolving a
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
// contract_events_daily uniqExact→uniqCombined(17) redesign: the whole
// point of using a uniq*-family aggregate (rather than
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

	if avail, definitive := er.DailyActivityAvailable(ctx); !avail || !definitive {
		t.Fatalf("DailyActivityAvailable = (%v,%v) after insert, want (true,true) — contract_events_daily MV did not populate", avail, definitive)
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

// TestNetworkThroughput_DedupsReingestedLedger guards that
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

// TestClickHouseContractDirectoryDedupsDuplicateEvents is the dedup proof for
// the two contract readers the original dedup sweep missed:
// RecentContracts and ContractInteractions. They appear in neither that
// sweep's fixed list nor its re-derived-as-correct list — they were simply
// not visited, while the sibling operations readers were fixed with LIMIT 1 BY.
//
// stellar.contract_events is ReplacingMergeTree(ingested_at), and duplicates
// are not hypothetical: the sink's own Flush contract is that a partially
// succeeded flush may be RETRIED over the same range, idempotent under RMT. So
// the table legitimately holds duplicate un-merged parts until a background
// merge collapses them — and both readers are window-scoped to recent ledgers,
// which is exactly where un-merged retries concentrate.
//
// This seeds the SAME event twice (a re-flush of the same range) and asserts
// the served counts do not double. It is a real-ClickHouse test rather than a
// query-shape assertion because the thing under test IS the engine's
// duplicate-row behaviour, which a stub cannot reproduce.
//
// Proven red: with count() restored in place of uniqExact, events comes back 2
// (want 1) and shared comes back 2 (want 1).
func TestClickHouseContractDirectoryDedupsDuplicateEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		baseLedger = uint32(73_500_001)
		subject    = "CDAT10_SUBJECT_AAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		partner    = "CDAT10_PARTNER_BBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
		txHash     = "7777777777777777777777777777777777777777777777777777777777777777"
	)
	closeTime := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)

	mkEvent := func(contractID string, eventIndex uint32) chstore.ContractEventRow {
		return chstore.ContractEventRow{
			LedgerSeq: baseLedger, CloseTime: closeTime, TxHash: txHash,
			OpIndex: 0, EventIndex: eventIndex, ContractID: contractID,
			EventType: "contract", TopicCount: 1, Topic0Sym: "transfer",
			TopicsXDR:        []string{scval.MustEncodeString("transfer")},
			DataXDR:          scval.MustEncodeString("body"),
			OpArgsXDR:        []string{},
			InSuccessfulCall: 1,
		}
	}
	// Subject and partner emit one event each, in the SAME tx — one shared tx.
	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{LedgerSeq: baseLedger, CloseTime: closeTime, ProtocolVersion: 22, SorobanEventCount: 2},
		Events: []chstore.ContractEventRow{mkEvent(subject, 0), mkEvent(partner, 1)},
	}

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	// Flush the SAME extract twice — a retried partial flush, byte-identical
	// rows under the RMT primary key, differing only in ingested_at.
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

	// ─── RecentContracts: the event tally must not double ────────────────
	dir, err := er.RecentContracts(ctx, 500, baseLedger)
	if err != nil {
		t.Fatalf("RecentContracts: %v", err)
	}
	var found bool
	for _, row := range dir {
		if row.ContractID != subject {
			continue
		}
		found = true
		if row.Events != 1 {
			t.Errorf("RecentContracts events for the subject = %d, want 1 — the same event "+
				"was flushed twice (a retried partial flush) and the un-merged duplicate "+
				"inflated the tally, mis-ranking the contracts directory (DAT-10)", row.Events)
		}
	}
	if !found {
		t.Fatalf("subject contract %s absent from RecentContracts", subject)
	}

	// ─── ContractInteractions: shared_txs must count TRANSACTIONS ────────
	edges, _, err := er.ContractInteractions(ctx, subject, 200, baseLedger)
	if err != nil {
		t.Fatalf("ContractInteractions: %v", err)
	}
	var edgeFound bool
	for _, e := range edges {
		if e.ContractID != partner {
			continue
		}
		edgeFound = true
		if e.SharedTxs != 1 {
			t.Errorf("shared_txs = %d, want 1 — the two contracts co-occur in exactly ONE "+
				"transaction. count() counted co-occurring EVENTS (and double-counted the "+
				"duplicate flush) despite the field being named shared_txs (DAT-10 + DAT-11 #50)",
				e.SharedTxs)
		}
	}
	if !edgeFound {
		t.Fatalf("partner contract %s absent from ContractInteractions", partner)
	}
}

// withEventTxs adds one stellar.transactions row per distinct (ledger, tx)
// among ext.Events, in first-seen order, as the extractor always writes them:
// the lake readers resolve each event's apply order from that table and fail
// a stream whose event has no transaction row.
func withEventTxs(ext chstore.LedgerExtract) chstore.LedgerExtract {
	next := map[uint32]uint32{}
	type ledgerTx struct {
		ledger uint32
		tx     string
	}
	seen := map[ledgerTx]bool{}
	for _, e := range ext.Events {
		k := ledgerTx{e.LedgerSeq, e.TxHash}
		if seen[k] {
			continue
		}
		seen[k] = true
		ext.Txs = append(ext.Txs, chstore.TransactionRow{
			LedgerSeq: e.LedgerSeq, CloseTime: e.CloseTime, TxHash: e.TxHash, TxIndex: next[e.LedgerSeq],
		})
		next[e.LedgerSeq]++
	}
	return ext
}

// TestSetCap67MovementsWatermark_OnlyAdvancesOverAProvenWindow pins the
// fail-closed half of the cap67 contiguity gate at the WRITE, where the
// invariant holds for every caller — Cap67Range's clamp only protects the one
// path that goes through it, and the range it resolves is resolved once per
// run while the watermark is the permanent record of what has been derived.
//
// The watermark is read back as max(thru_ledger) and the derive resumes at
// watermark+1 with no trailing re-derive, so an advance over an unproven
// ledger drops that ledger's account movements permanently and invisibly.
// Both refusals are therefore delay, not failure: the lake heals via
// ch-live-catchup and the next run re-derives the window.
//
// Proven red on the unfixed tree (revert the cap67AdvanceProven call in
// SetCap67MovementsWatermark): step 2 records base+10 over the hole at base+6
// and step 4 records base+10 over the never-derived [base+6, base+7].
//
// Note: the guard widens
// SetCap67MovementsWatermark to take the window's lower bound, so this file
// does not compile against the unfixed signature — the wholesale-revert red
// proof lives in clickhouse_cap67_to_clamp_test.go, which drives the same
// defect through the CLI using only unchanged signatures.
func TestSetCap67MovementsWatermark_OnlyAdvancesOverAProvenWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// An isolated ledger range nothing else in the suite writes to, below the
	// suite's global-tip claimants — see the note in
	// clickhouse_cap67_to_clamp_test.go.
	const base = uint32(160_700_000)

	cap67TruncateWatermark(t)
	t.Cleanup(func() { cap67TruncateWatermark(t) })

	// Present [base, base+5], HOLE at base+6, present [base+7, base+10].
	var seqs []uint32
	for seq := base; seq <= base+5; seq++ {
		seqs = append(seqs, seq)
	}
	for seq := base + 7; seq <= base+10; seq++ {
		seqs = append(seqs, seq)
	}
	cap67SeedLedgers(t, ctx, addr, seqs)

	readWM := func(what string) uint32 {
		t.Helper()
		wm, err := chstore.Cap67MovementsWatermark(ctx, addr)
		if err != nil {
			t.Fatalf("Cap67MovementsWatermark (%s): %v", what, err)
		}
		return wm
	}

	// 1. A window straddling the hole must be REFUSED, and refused by kind so
	//    the follow loop can read it as "delayed", not "broken".
	err := chstore.SetCap67MovementsWatermark(ctx, addr, base, base+10)
	if !errors.Is(err, chstore.ErrCap67MovementsHole) {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) over a hole at %d = %v, want ErrCap67MovementsHole",
			base, base+10, base+6, err)
	}
	if wm := readWM("after the refused hole window"); wm != 0 {
		t.Fatalf("watermark = %d after a REFUSED advance, want 0 (unmoved) — recording %d would strand "+
			"ledger %d: max(thru_ledger) never walks back and the derive resumes at watermark+1",
			wm, base+10, base+6)
	}

	// 2. Non-vacuity: the hole-free prefix of the same range DOES advance.
	if err := chstore.SetCap67MovementsWatermark(ctx, addr, base, base+5); err != nil {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) over a contiguous window: %v (the guard must pass "+
			"proven work, not refuse everything)", base, base+5, err)
	}
	if wm := readWM("after the proven window"); wm != base+5 {
		t.Fatalf("watermark = %d, want %d", wm, base+5)
	}

	// 3. A window starting above watermark+1 must be REFUSED even though the
	//    window itself is hole-free: [base+6, base+7] was never derived.
	err = chstore.SetCap67MovementsWatermark(ctx, addr, base+8, base+10)
	if !errors.Is(err, chstore.ErrCap67MovementsSkippedPrefix) {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) with the watermark at %d = %v, want "+
			"ErrCap67MovementsSkippedPrefix", base+8, base+10, base+5, err)
	}
	if wm := readWM("after the refused skip"); wm != base+5 {
		t.Fatalf("watermark = %d after a REFUSED skip, want %d (unmoved)", wm, base+5)
	}

	// 4. Heal the hole; the resume window is now proven and advances.
	cap67SeedLedgers(t, ctx, addr, []uint32{base + 6})
	if err := chstore.SetCap67MovementsWatermark(ctx, addr, base+6, base+10); err != nil {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) after healing: %v", base+6, base+10, err)
	}
	if wm := readWM("after healing"); wm != base+10 {
		t.Fatalf("watermark = %d, want %d", wm, base+10)
	}

	// 5. An idempotent re-derive BELOW the watermark is neither refused nor a
	//    walk-back — account_movements is a ReplacingMergeTree and operators
	//    do top up old ranges.
	if err := chstore.SetCap67MovementsWatermark(ctx, addr, base, base+5); err != nil {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) below the watermark: %v, want nil", base, base+5, err)
	}
	if wm := readWM("after a re-derive below the watermark"); wm != base+10 {
		t.Fatalf("watermark = %d after re-deriving [%d,%d], want %d (unchanged)", wm, base, base+5, base+10)
	}
}

// TestSetCap67MovementsWatermark_RefusesAnEventShortfall pins the event half
// of the proof: a window whose ledgers are all present but whose
// contract_events fall short of Σ soroban_event_count (a dropped or unrestored
// partition) must not advance, and advances once the events are back.
func TestSetCap67MovementsWatermark_RefusesAnEventShortfall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const base = uint32(160_710_000)
	cap67TruncateWatermark(t)
	t.Cleanup(func() { cap67TruncateWatermark(t) })

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	closeTime := time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC)
	ledger := func(seq, events uint32) chstore.LedgerExtract {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: closeTime, SorobanEventCount: events,
			LedgerHash: "aa04", PrevHash: "bb04", ProtocolVersion: 23, BucketListHash: "cc04",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		for i := range events {
			ext.Events = append(ext.Events, chstore.ContractEventRow{
				LedgerSeq: seq, CloseTime: closeTime, TxHash: "dd04", EventIndex: i,
				ContractID: "CAAA", EventType: "contract", InSuccessfulCall: 1,
			})
		}
		return ext
	}
	flush := func(exts ...chstore.LedgerExtract) {
		t.Helper()
		for _, ext := range exts {
			if err := sink.Add(ctx, ext); err != nil {
				t.Fatalf("sink add ledger %d: %v", ext.Ledger.LedgerSeq, err)
			}
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}

	// Ledger base+2 declares two events that contract_events does not hold.
	shortfall := ledger(base+2, 2)
	shortfall.Events = nil
	flush(ledger(base, 0), ledger(base+1, 0), shortfall, ledger(base+3, 0))

	err = chstore.SetCap67MovementsWatermark(ctx, addr, base, base+3)
	if !errors.Is(err, chstore.ErrCap67MovementsEventShortfall) {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) missing ledger %d's events = %v, want ErrCap67MovementsEventShortfall",
			base, base+3, base+2, err)
	}
	if wm, err := chstore.Cap67MovementsWatermark(ctx, addr); err != nil || wm != 0 {
		t.Fatalf("watermark = %d (%v) after a REFUSED advance, want 0 (unmoved)", wm, err)
	}

	// Restore the events; the same window is now proven and advances.
	flush(ledger(base+2, 2))
	if err := chstore.SetCap67MovementsWatermark(ctx, addr, base, base+3); err != nil {
		t.Fatalf("SetCap67MovementsWatermark([%d,%d]) with events restored: %v", base, base+3, err)
	}
	if wm, err := chstore.Cap67MovementsWatermark(ctx, addr); err != nil || wm != base+3 {
		t.Fatalf("watermark = %d (%v), want %d", wm, err, base+3)
	}
}

// TestCap67SupplyCoverage_GrowsOnlyOverProvenAdjacentWindows pins the
// supply-kind range the movements note reports against the real table: no
// rows read as no range (not [0, 0] covering genesis), the first window
// starts it, adjacent windows extend it, and a window over a lake hole is
// refused without moving it.
func TestCap67SupplyCoverage_GrowsOnlyOverProvenAdjacentWindows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Isolated from the other cap67 tests' ranges.
	const base = uint32(160_720_000)

	cap67TruncateWatermark(t)
	t.Cleanup(func() { cap67TruncateWatermark(t) })

	// Present [base, base+2], HOLE at base+3, present [base+4, base+20].
	var seqs []uint32
	for seq := base; seq <= base+20; seq++ {
		if seq != base+3 {
			seqs = append(seqs, seq)
		}
	}
	cap67SeedLedgers(t, ctx, addr, seqs)

	readRange := func(what string) (uint32, uint32, bool) {
		t.Helper()
		cov, err := chstore.Cap67MovementsCoverage(ctx, addr)
		if err != nil {
			t.Fatalf("Cap67MovementsCoverage (%s): %v", what, err)
		}
		return cov.SupplyRange()
	}
	extend := func(lo, hi uint32) error {
		t.Helper()
		return chstore.ExtendCap67SupplyCoverage(ctx, addr, lo, hi)
	}

	if from, thru, ok := readRange("empty"); ok {
		t.Fatalf("empty table: supply range = [%d,%d], want none", from, thru)
	}

	if err := chstore.SetCap67MovementsWatermark(ctx, addr, base+4, base+20); err != nil {
		t.Fatalf("SetCap67MovementsWatermark: %v", err)
	}
	if err := extend(base+15, base+20); err != nil {
		t.Fatalf("first window: %v", err)
	}
	if err := extend(base+10, base+14); err != nil {
		t.Fatalf("adjacent window below: %v", err)
	}
	if from, thru, ok := readRange("after two windows"); !ok || from != base+10 || thru != base+20 {
		t.Fatalf("supply range = [%d,%d] ok=%v, want [%d,%d]", from, thru, ok, base+10, base+20)
	}

	if err := extend(base+2, base+9); !errors.Is(err, chstore.ErrCap67MovementsHole) {
		t.Fatalf("window over the hole at %d = %v, want ErrCap67MovementsHole", base+3, err)
	}
	if from, _, _ := readRange("after refusal"); from != base+10 {
		t.Fatalf("a refused window moved the supply floor to %d, want %d", from, base+10)
	}

	if err := extend(base+4, base+9); err != nil {
		t.Fatalf("contiguous window below: %v", err)
	}
	if from, thru, ok := readRange("after backfill"); !ok || from != base+4 || thru != base+20 {
		t.Fatalf("supply range = [%d,%d] ok=%v, want [%d,%d]", from, thru, ok, base+4, base+20)
	}
	if wm, err := chstore.Cap67MovementsWatermark(ctx, addr); err != nil || wm != base+20 {
		t.Fatalf("main watermark = %d (%v), want %d: the supply rows must not move it", wm, err, base+20)
	}
}

// This file is the SILENT-DATA-LOSS proof for the cap67 movements watermark,
// driven through the production entry point (chops.Run — the same dispatch
// cmd/stellarindex-ops uses) rather than through a helper: the defect lives in
// how the CLI's own flags reach the range resolver, so a test that called the
// resolver directly would never see it.
//
// The defect, in two shapes of one disease — the watermark advancing to a
// ledger whose coverage was never proven:
//
//  1. `-to N` bypassed the contiguity gate. The ContiguousWatermark clamp sat
//     INSIDE the `to == 0` branch of Cap67Range, so an operator-supplied upper
//     bound was trusted verbatim: the derive read straight past a near-tip lake
//     hole and stamped the watermark above it at every window top.
//  2. `-from N` above watermark+1 advanced the watermark over ledgers the run
//     never derived at all.
//
// Either way the loss is PERMANENT and invisible: the watermark is read back as
// max(thru_ledger), the derive resumes at watermark+1 with no trailing
// re-derive, and the /movements handler floors its Postgres arm at the same
// watermark — so the skipped ledgers' classic/native movements are served by
// neither arm, forever. The raw lake heals itself via ch-live-catchup;
// account_movements never revisits.

// cap67TruncateWatermark restores the single-row watermark table to its
// pristine (never-run) state. Called BEFORE and AFTER each test here so these
// tests are order-independent, and so
// TestCap67Range_FirstRunClampsToTheLakesFirstLedger's "this is a first run"
// precondition still holds whichever of us the runner reaches first.
//
// It owns its context rather than borrowing the test's: it runs from
// t.Cleanup, which fires AFTER the test body's `defer cancel()`, so a borrowed
// context is already cancelled and the restore would silently not happen.
func cap67TruncateWatermark(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := dialClickHouse(t, ctx, "stellar")
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.cap67_movements_watermark`); err != nil {
		t.Fatalf("truncate cap67 watermark: %v", err)
	}
}

// cap67SeedLedgers writes the given ledger sequences through the repo's own
// sink, which is what makes them "present in the lake" for every contiguity
// query: stellar.ledgers is the per-ledger commit marker Sink.Flush writes last.
func cap67SeedLedgers(t *testing.T, ctx context.Context, addr string, seqs []uint32) {
	t.Helper()
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	for _, seq := range seqs {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC),
			LedgerHash: "aa03", PrevHash: "bb03", ProtocolVersion: 23, BucketListHash: "cc03",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush ledger seed: %v", err)
	}
}

// TestCap67Movements_ExplicitToIsClampedToTheContiguousTip runs the real
// subcommand with `-to` pointed ABOVE a lake hole and proves the watermark
// stops BELOW the hole.
//
// Proven red on the unfixed tree: with the ContiguousWatermark clamp back
// inside Cap67Range's `if last == 0` branch, the run derives [base, base+12]
// and the watermark reads base+12 — ledger base+6's movements are then gone
// for good.
func TestCap67Movements_ExplicitToIsClampedToTheContiguousTip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// An isolated ledger range nothing else in the suite writes to, kept
	// BELOW the two tests that claim the lake's global tip
	// (TestNetworkThroughput_DedupsReingestedLedger at 200M,
	// TestSDEXOrderBook_ConvergesAfterLakeHoleIsFilled at 217M) — and with a
	// close_time below theirs, since NetworkThroughput anchors its day window
	// to max(close_time) over the whole table. The assertions here need only
	// a hole above `base`, never the global maximum.
	const base = uint32(160_500_000)

	cap67TruncateWatermark(t)
	t.Cleanup(func() { cap67TruncateWatermark(t) })

	// Present [base, base+5], HOLE at base+6, present [base+7, base+12] —
	// the near-tip LiveSink bounded-drop shape.
	var seqs []uint32
	for seq := base; seq <= base+5; seq++ {
		seqs = append(seqs, seq)
	}
	for seq := base + 7; seq <= base+12; seq++ {
		seqs = append(seqs, seq)
	}
	cap67SeedLedgers(t, ctx, addr, seqs)

	// The operator's post-incident invocation: an explicit window straight
	// across the hole.
	if err := chops.Run([]string{
		"ch-cap67-movements",
		"-ch-addr", addr,
		"-from", strconv.FormatUint(uint64(base), 10),
		"-to", strconv.FormatUint(uint64(base+12), 10),
		"-write",
	}); err != nil {
		t.Fatalf("ch-cap67-movements -from %d -to %d: %v (the clamp must DELAY the derive, not fail it)",
			base, base+12, err)
	}

	wm, err := chstore.Cap67MovementsWatermark(ctx, addr)
	if err != nil {
		t.Fatalf("Cap67MovementsWatermark: %v", err)
	}
	if wm != base+5 {
		t.Fatalf("watermark after `-to %d` across a hole at %d = %d, want %d (the contiguous tip). "+
			"A watermark above %d means the derive stepped past the hole and those ledgers' account "+
			"movements are lost permanently: the next run resumes at watermark+1 and the /movements "+
			"handler floors its Postgres arm at this value.", base+12, base+6, wm, base+5, base+6)
	}

	// The resolver itself, for a direct read of the same invariant.
	start, last, err := chops.Cap67Range(ctx, addr, base, base+12, 1)
	if err != nil {
		t.Fatalf("Cap67Range: %v", err)
	}
	if start != base || last != base+5 {
		t.Fatalf("Cap67Range(from=%d, to=%d) = [%d,%d], want [%d,%d] — an explicit -to must be min()'d "+
			"against the contiguous tip, not trusted", base, base+12, start, last, base, base+5)
	}

	// Non-vacuity: heal the hole and the same invocation now advances to the
	// top of the requested window, proving the clamp tracks the lake rather
	// than refusing everything.
	cap67SeedLedgers(t, ctx, addr, []uint32{base + 6})
	if err := chops.Run([]string{
		"ch-cap67-movements",
		"-ch-addr", addr,
		"-to", strconv.FormatUint(uint64(base+12), 10),
		"-write",
	}); err != nil {
		t.Fatalf("ch-cap67-movements -to %d after healing: %v", base+12, err)
	}
	healed, err := chstore.Cap67MovementsWatermark(ctx, addr)
	if err != nil {
		t.Fatalf("Cap67MovementsWatermark (healed): %v", err)
	}
	if healed != base+12 {
		t.Fatalf("watermark after healing the hole = %d, want %d — once the lake is whole the derive "+
			"must catch up through the requested -to", healed, base+12)
	}
}

// TestCap67Movements_RefusesToAdvanceOverUnderivedLedgers is the sibling shape
// of the same defect: `-from` above watermark+1. The window itself is
// hole-free, so the contiguity clamp has nothing to say — yet stamping the
// watermark at that window's top CLAIMS the ledgers between the old watermark
// and `-from` as derived when the run never read them.
//
// Proven red on the unfixed tree: the second run returns nil and the watermark
// jumps to base+10, silently orphaning [base+4, base+5].
func TestCap67Movements_RefusesToAdvanceOverUnderivedLedgers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Below the suite's global-tip claimants — see the note on the previous
	// test's base.
	const base = uint32(160_600_000)

	cap67TruncateWatermark(t)
	t.Cleanup(func() { cap67TruncateWatermark(t) })

	var seqs []uint32
	for seq := base; seq <= base+10; seq++ {
		seqs = append(seqs, seq)
	}
	cap67SeedLedgers(t, ctx, addr, seqs)

	// A normal run establishes the prefix [base, base+3].
	if err := chops.Run([]string{
		"ch-cap67-movements",
		"-ch-addr", addr,
		"-from", strconv.FormatUint(uint64(base), 10),
		"-to", strconv.FormatUint(uint64(base+3), 10),
		"-write",
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	wm, err := chstore.Cap67MovementsWatermark(ctx, addr)
	if err != nil {
		t.Fatalf("Cap67MovementsWatermark: %v", err)
	}
	if wm != base+3 {
		t.Fatalf("watermark after the seed run = %d, want %d", wm, base+3)
	}

	// The over-eager resume: skip [base+4, base+5] entirely.
	if err := chops.Run([]string{
		"ch-cap67-movements",
		"-ch-addr", addr,
		"-from", strconv.FormatUint(uint64(base+6), 10),
		"-to", strconv.FormatUint(uint64(base+10), 10),
		"-write",
	}); err == nil {
		t.Fatalf("ch-cap67-movements -from %d with the watermark at %d returned nil — it must REFUSE: "+
			"advancing claims [%d,%d] as derived when this run skipped them", base+6, base+3, base+4, base+5)
	}

	after, err := chstore.Cap67MovementsWatermark(ctx, addr)
	if err != nil {
		t.Fatalf("Cap67MovementsWatermark (after refusal): %v", err)
	}
	if after != base+3 {
		t.Fatalf("watermark after the refused run = %d, want %d (unmoved) — a refusal that still "+
			"advances the watermark loses [%d,%d] permanently", after, base+3, base+4, base+5)
	}

	// Non-vacuity: the correct resume (no -from, i.e. watermark+1) works and
	// carries the watermark to the contiguous tip.
	if err := chops.Run([]string{
		"ch-cap67-movements",
		"-ch-addr", addr,
		"-to", strconv.FormatUint(uint64(base+10), 10),
		"-write",
	}); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	resumed, err := chstore.Cap67MovementsWatermark(ctx, addr)
	if err != nil {
		t.Fatalf("Cap67MovementsWatermark (resumed): %v", err)
	}
	if resumed != base+10 {
		t.Fatalf("watermark after the correct resume = %d, want %d", resumed, base+10)
	}
}

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

	// Index-path authority needs the coverage marker; without it the reader
	// answers from the bloom scan and these assertions would not test the index.
	t.Cleanup(func() { _ = conn.Exec(context.Background(), `TRUNCATE TABLE stellar.tx_hash_index_coverage`) })
	if err := chstore.MarkTxHashIndexCovered(ctx, addr, ledger, ledger); err != nil {
		t.Fatalf("mark coverage (mv): %v", err)
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
	if err := chstore.MarkTxHashIndexCovered(ctx, addr, ledger, ledger); err != nil {
		t.Fatalf("mark coverage (backfill): %v", err)
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

// TestEventCensusShortfalls_DroppedEventPartition is the proof on a real
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
	// Left behind, these 2027/08/01 rows own the lake's max(close_time), which
	// anchors clickhouse_storage_test's NetworkThroughput day window past its ledger.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		conn := dialClickHouse(t, cctx, "stellar")
		if err := conn.Exec(cctx, fmt.Sprintf(`ALTER TABLE stellar.ledgers DELETE
			WHERE ledger_seq BETWEEN %d AND %d SETTINGS mutations_sync = 2`, p1-3, p1+2)); err != nil {
			t.Errorf("purge census fixture ledgers: %v", err)
		}
	})

	// Stay below the 2027/06/15 close_time TestNetworkThroughput_DedupsReingestedLedger
	// reserves as the global tip; this file runs before it when both share a shard.
	seed := func(seq, events uint32) {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
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

// TestVerifyLake_RawCensusCatchesDroppedTablePartition proves verify-lake's
// raw-table census on a real ClickHouse: a DROP PARTITION on a non-ledger raw
// table leaves stellar.ledgers contiguous and hash-chained, so SubstrateProblem
// (the contiguity + hash-chain population) still reports the range clean, and
// only the census sees the rows are gone.
func TestVerifyLake_RawCensusCatchesDroppedTablePartition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Partitions 148/149: nothing else in the suite writes there.
	const lo, hi = uint32(148_999_997), uint32(149_000_002)
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		conn := dialClickHouse(t, cctx, "stellar")
		for _, table := range []string{"ledgers", "transactions", "operations", "operation_results", "operation_participants", "contract_events"} {
			if err := conn.Exec(cctx, fmt.Sprintf(`ALTER TABLE stellar.%s DELETE
				WHERE ledger_seq BETWEEN %d AND %d SETTINGS mutations_sync = 2`, table, lo, hi)); err != nil {
				t.Errorf("purge raw census fixture %s: %v", table, err)
			}
		}
	})

	// Stay below the 2027/06/15 close_time TestNetworkThroughput_DedupsReingestedLedger
	// reserves as the global tip.
	closeTime := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for seq := lo; seq <= hi; seq++ {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: closeTime,
			LedgerHash: fmt.Sprintf("h%d", seq), PrevHash: fmt.Sprintf("h%d", seq-1),
			ProtocolVersion: 22, BucketListHash: "cc00",
			TxCount: 2, OpCount: 3, SorobanEventCount: 2,
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		// Distinct sort keys per row, so a merge cannot collapse fixture rows.
		for tx := uint32(0); tx < 2; tx++ {
			ext.Txs = append(ext.Txs, chstore.TransactionRow{
				LedgerSeq: seq, CloseTime: closeTime, TxHash: fmt.Sprintf("rawcensus%d-%d", seq, tx),
				TxIndex: tx, SourceAccount: "GRAWCENSUSFIXTURE", OperationCount: 1, Successful: 1,
			})
		}
		for op := uint32(0); op < 3; op++ {
			txHash := fmt.Sprintf("rawcensus%d-0", seq)
			ext.Ops = append(ext.Ops, chstore.OperationRow{
				LedgerSeq: seq, CloseTime: closeTime, TxHash: txHash, OpIndex: op, OpType: "payment",
				SourceAccount: "GRAWCENSUSFIXTURE",
			})
			ext.Results = append(ext.Results, chstore.OperationResultRow{LedgerSeq: seq, TxHash: txHash, OpIndex: op})
		}
		ext.Participants = append(ext.Participants, chstore.OperationParticipantRow{
			Account: "GRAWCENSUSPARTICIPANT", LedgerSeq: seq, CloseTime: closeTime,
			TxHash: fmt.Sprintf("rawcensus%d-0", seq),
		})
		for ev := uint32(0); ev < 2; ev++ {
			ext.Events = append(ext.Events, chstore.ContractEventRow{
				LedgerSeq: seq, CloseTime: closeTime, TxHash: fmt.Sprintf("rawcensus%d-1", seq),
				EventIndex: ev, ContractID: "CRAWCENSUSFIXTURE", EventType: "contract",
				TopicCount: 1, Topic0Sym: "transfer", InSuccessfulCall: 1,
			})
		}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	textfile := filepath.Join(t.TempDir(), "lake_verify.prom")
	run := func() error {
		return chops.Run([]string{
			"verify-lake", "-checks", "rawcensus",
			"-config", filepath.Join(t.TempDir(), "absent.toml"), "-ch-addr", addr,
			"-from", fmt.Sprint(lo), "-to", fmt.Sprint(hi), "-textfile", textfile,
		})
	}
	assertExit := func(t *testing.T, stage string, err error, code int, failures string) {
		t.Helper()
		var exit *opsutil.ExitCodeError
		if !errors.As(err, &exit) || exit.Code != code {
			t.Fatalf("%s: verify-lake err = %v, want ExitCodeError{Code:%d}", stage, err, code)
		}
		body, rerr := os.ReadFile(textfile)
		if rerr != nil {
			t.Fatalf("%s: read textfile: %v", stage, rerr)
		}
		if want := `stellarindex_lake_verify_failures{check="raw_census"} ` + failures + "\n"; !strings.Contains(string(body), want) {
			t.Fatalf("%s: textfile missing %q:\n%s", stage, want, body)
		}
	}

	// (a) intact fixture passes.
	if err := run(); err != nil {
		t.Fatalf("verify-lake over the intact fixture = %v, want nil", err)
	}

	conn := dialClickHouse(t, ctx, "stellar")
	if err := conn.Exec(ctx, "ALTER TABLE stellar.operations DROP PARTITION 149"); err != nil {
		t.Fatalf("drop operations partition: %v", err)
	}

	// (b) the pre-census population cannot see it.
	if p, has, d, err := chstore.SubstrateProblem(ctx, addr, lo, hi); err != nil || has {
		t.Fatalf("SubstrateProblem after the operations drop = (%d, %v, %q, %v); this test assumes ledgers stay intact", p, has, d, err)
	}

	// (c) the census does.
	assertExit(t, "operations dropped", run(), 1, "1")

	// (d) a presence-only table's loss is reported as such.
	t.Run("presence-only participants drop", func(t *testing.T) {
		if err := conn.Exec(ctx, "ALTER TABLE stellar.operation_participants DROP PARTITION 149"); err != nil {
			t.Fatalf("drop operation_participants partition: %v", err)
		}
		short, _, err := chstore.RawTableCensus(ctx, addr, lo, hi)
		if err != nil {
			t.Fatalf("RawTableCensus: %v", err)
		}
		want := []chstore.RawTableShortfall{
			{Table: "operations", Partition: 149, Expected: 9, Present: 0},
			{Table: "operation_participants", Partition: 149, Expected: 1, Present: 0},
		}
		if !reflect.DeepEqual(short, want) {
			t.Fatalf("shortfalls = %+v, want %+v", short, want)
		}
		assertExit(t, "participants dropped", run(), 2, "2")
	})
}

// insertContiguityLedgers writes one tx-bearing stellar.ledgers row per seq,
// each through its own sink flush so a repeated seq lands as a separate,
// un-merged ReplacingMergeTree part.
func insertContiguityLedgers(ctx context.Context, t *testing.T, addr string, seqs ...uint32) {
	t.Helper()
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	for _, seq := range seqs {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC),
			LedgerHash: fmt.Sprintf("h%d", seq), PrevHash: fmt.Sprintf("h%d", seq-1),
			ProtocolVersion: 11, TxCount: 1, OpCount: 1,
		}}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add %d: %v", seq, err)
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("sink flush %d: %v", seq, err)
		}
	}
}

func contiguityECRow(seq uint32, txHash string) chstore.LedgerEntryChangeRow {
	return chstore.LedgerEntryChangeRow{
		LedgerSeq: seq, CloseTime: time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC),
		TxHash: txHash, OpIndex: -1, ChangeType: "updated", EntryType: "account",
		KeyXDR: fmt.Sprintf("k%d", seq), EntryXDR: fmt.Sprintf("e%d", seq),
	}
}

// TestQueryLedgerRangeCoverage_SeesUnmergedDuplicates pins that Check 1's
// headline reads count() alongside uniqExact: a re-ingested ledger left
// un-merged is visible as DuplicateRows, where uniqExact alone reported the
// range as "every ledger present exactly once".
func TestQueryLedgerRangeCoverage_SeesUnmergedDuplicates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Hold merges so the duplicate part cannot collapse before the read.
	conn := dialClickHouse(t, ctx, "stellar")
	if err := conn.Exec(ctx, "SYSTEM STOP MERGES stellar.ledgers"); err != nil {
		t.Fatalf("stop merges: %v", err)
	}
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "SYSTEM START MERGES stellar.ledgers") })

	const base = uint32(7_310_000)
	insertContiguityLedgers(ctx, t, addr, base, base+1, base+1, base+3)

	got, err := chstore.QueryLedgerRangeCoverage(ctx, addr, base, base+3)
	if err != nil {
		t.Fatalf("QueryLedgerRangeCoverage: %v", err)
	}
	if got.Expected != 4 || got.Present != 3 || got.Missing() != 1 {
		t.Errorf("coverage = expected %d present %d missing %d, want 4/3/1", got.Expected, got.Present, got.Missing())
	}
	if got.Rows != 4 || got.DuplicateRows() != 1 {
		t.Errorf("rows=%d duplicate_rows=%d, want 4/1 — the un-merged re-ingest of %d is invisible to Check 1", got.Rows, got.DuplicateRows(), base+1)
	}
}

// TestQueryECWindowCoverage_SeedOnlyLedgerIsAGap pins that a tx-bearing
// ledger whose only entry-change row is a state-snapshot seed counts as
// uncovered: the seed proves the entry's state, not that the ledger's tx meta
// was captured.
func TestQueryECWindowCoverage_SeedOnlyLedgerIsAGap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const base = uint32(7_320_000)
	seedOnly := base + 2
	insertContiguityLedgers(ctx, t, addr, base, base+1, base+2, base+3)
	seed, ok := chstore.SnapshotEntryRow(&xdr.LedgerEntry{
		LastModifiedLedgerSeq: xdr.Uint32(seedOnly),
		Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount, Account: &xdr.AccountEntry{
			AccountId: xdr.MustAddress("GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7"), Balance: 1, Thresholds: xdr.Thresholds{1, 0, 0, 0},
		}},
	}, time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC))
	if !ok || seed.LedgerSeq != seedOnly || seed.TxHash != "" {
		t.Fatalf("SnapshotEntryRow = (%+v, %v), want an empty-tx_hash seed at %d", seed, ok, seedOnly)
	}
	ec := []chstore.LedgerEntryChangeRow{
		contiguityECRow(base, fmt.Sprintf("tx%d", base)),
		contiguityECRow(base+1, fmt.Sprintf("tx%d", base+1)),
		seed,
		contiguityECRow(base+3, fmt.Sprintf("tx%d", base+3)),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, ec, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	got, err := chstore.QueryECWindowCoverage(ctx, addr, base, base+3, 100)
	if err != nil {
		t.Fatalf("QueryECWindowCoverage: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("QueryECWindowCoverage returned %d windows, want 1: %+v", len(got), got)
	}
	if w := got[0]; w.TxLedgers != 4 || w.ECCoveredTxLedgers != 3 || w.Missing() != 1 {
		t.Errorf("window = tx %d covered %d missing %d, want 4/3/1 — the seed-only ledger %d was counted as covered", w.TxLedgers, w.ECCoveredTxLedgers, w.Missing(), seedOnly)
	}
}

// TestVerifyContiguity_AutoECFloorGatesHoleBelowOldConstant is the
// end-to-end proof. Ledgers [7,300,000, 7,300,009] are tx-bearing with
// transaction-scoped entry-change rows on all but 7,300,005. That band sits
// far below the old hardcoded -ec-floor (63,050,000), which routed the hole
// to the informational arm and exited 0. With the floor derived from the
// lake's own coverage edge the hole is a hard deficiency (exit 1).
//
// Ledgers [7,299,990, 7,299,999] precede the edge with only a snapshot seed
// row (empty tx_hash) at 7,299,995: the edge must ignore it, so exactly one
// deficiency is reported, not six.
func TestVerifyContiguity_AutoECFloorGatesHoleBelowOldConstant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const from, edge, hole, to = uint32(7_299_990), uint32(7_300_000), uint32(7_300_005), uint32(7_300_009)
	var seqs []uint32
	var ec []chstore.LedgerEntryChangeRow
	for seq := from; seq <= to; seq++ {
		seqs = append(seqs, seq)
		if seq >= edge && seq != hole {
			ec = append(ec, contiguityECRow(seq, fmt.Sprintf("tx%d", seq)))
		}
	}
	ec = append(ec, contiguityECRow(7_299_995, ""))
	insertContiguityLedgers(ctx, t, addr, seqs...)
	if _, err := chstore.InsertEntryChanges(ctx, addr, ec, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	gotEdge, found, err := chstore.QueryECLowerEdge(ctx, addr, from, to)
	if err != nil {
		t.Fatalf("QueryECLowerEdge: %v", err)
	}
	if !found || gotEdge != edge {
		t.Fatalf("QueryECLowerEdge = (%d, %v), want (%d, true) — a snapshot row must not lower the edge", gotEdge, found, edge)
	}
	if _, found, err := chstore.QueryECLowerEdge(ctx, addr, to+1_000_000, to+1_000_100); err != nil || found {
		t.Fatalf("QueryECLowerEdge over an empty range = (found %v, err %v), want (false, nil)", found, err)
	}

	err = chops.Run([]string{
		"verify-contiguity", "-check", "entrychanges",
		"-config", filepath.Join(t.TempDir(), "absent.toml"), "-ch-addr", addr,
		"-from", fmt.Sprint(from), "-to", fmt.Sprint(to),
	})
	var exit *opsutil.ExitCodeError
	if !errors.As(err, &exit) {
		t.Fatalf("verify-contiguity err = %v, want ExitCodeError{Code:1} — the hole at %d was exempted from the hard gate", err, hole)
	}
	if exit.Code != 1 {
		t.Fatalf("verify-contiguity exit code = %d, want 1 (exactly the hole at %d)", exit.Code, hole)
	}
}

// TestContiguousWatermark_HoleAtFrom is the HIGH silent-data-loss proof for the
// CH-source projector's anti-skip guard (ADR-0034 #10). ContiguousWatermark is
// the projector's safe upper read bound: it must return from-1 (stall) when the
// lake has a hole AT the lower boundary `from`, so the projector never scans past
// the missing ledger and upserts its cursor beyond it — which would permanently
// drop that ledger's projected sole-writer sep41 mint/burn/transfer rows from the
// served tier.
//
// The bug: the watermark's interior-gap scan (leadInFrame over DISTINCT
// ledger_seq >= from) is blind to a hole exactly at `from`. When `from` is
// absent, the smallest present ledger is from+1 and {from+1, from+2, …} is
// internally contiguous, so the gap scan returns 0 ("no hole") and a naive
// code returned chMax — advancing the projector RIGHT OVER the missing ledger.
//
// This is a real-ClickHouse test (not a query-shape assertion) because the thing
// under test IS what the SQL actually produces for a missing `from`: the fix adds
// a min(ledger_seq >= from) column, and the only way to prove the SQL yields
// min_present == from+1 (not a synthetic firstGap) for a genuine boundary hole is
// to seed one and read it back through the real engine.
//
// Proven red: with the fix reverted (min_present guard removed), the hole-at-from
// query returns chMax (from+5), not from-1.
func TestContiguousWatermark_HoleAtFrom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// An isolated, high ledger range so this test's rows are the global max —
	// ContiguousWatermark's ch_max is max() over the WHOLE ledgers table, and the
	// gap/min scans are scoped to >= from. Nothing else in the suite writes here.
	const from = uint32(215_000_000)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	seed := func(seq uint32) {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC),
			LedgerHash: "aa00", PrevHash: "bb00", ProtocolVersion: 22, BucketListHash: "cc00",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}

	// Seed [from+1, from+5] — deliberately SKIP `from` itself so there is a hole
	// exactly at the lower boundary. The present set is internally contiguous, so
	// the interior-gap scan reports "no hole".
	for seq := from + 1; seq <= from+5; seq++ {
		seed(seq)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush hole seed: %v", err)
	}

	// Hole at `from` ⟹ watermark must STALL at from-1, not advance to chMax.
	wm, err := chstore.ContiguousWatermark(ctx, addr, from)
	if err != nil {
		t.Fatalf("ContiguousWatermark(hole at from): %v", err)
	}
	if wm != from-1 {
		t.Fatalf("ContiguousWatermark(from=%d) with a hole AT from = %d, want %d (from-1). "+
			"A value >= from means the projector would scan past the missing ledger and drop its "+
			"sole-writer sep41 rows.", from, wm, from-1)
	}

	// Heal the hole: seed `from` itself. Now [from, from+5] is contiguous, so the
	// watermark advances past the boundary — proving the stall was caused by the
	// hole and not a blanket refusal to advance.
	seed(from)
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush heal seed: %v", err)
	}
	healed, err := chstore.ContiguousWatermark(ctx, addr, from)
	if err != nil {
		t.Fatalf("ContiguousWatermark(healed): %v", err)
	}
	if healed < from+5 {
		t.Fatalf("ContiguousWatermark(from=%d) after healing = %d, want >= %d "+
			"(the contiguous run [from, from+5] should let it advance)", from, healed, from+5)
	}
}

// TestCap67Range_StallsAtHole is the money-display correctness proof for the
// cap67 movements derive: its upper bound MUST clamp to the contiguous
// watermark, not the raw lake max. The LiveSink drops whole ledgers under
// pressure, so a near-tip hole can exist; a naive derive read to
// MaxLedger and advanced its watermark PAST the hole with no trailing
// re-derive — permanently dropping that ledger's classic/native account
// movements. This proves Cap67Range now STALLS before an interior hole.
//
// Red-proof: revert the ContiguousWatermark call in Cap67Range back to
// clickhouse.MaxLedger and this fails — `last` jumps to the global max
// (>= from+10), i.e. the derive would step past the hole at from+6.
func TestCap67Range_StallsAtHole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Isolated high range, distinct from the other watermark test.
	const from = uint32(216_000_000)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	seed := func(seq uint32) {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: time.Date(2027, 8, 1, 0, 0, 0, 0, time.UTC),
			LedgerHash: "aa01", PrevHash: "bb01", ProtocolVersion: 22, BucketListHash: "cc01",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}
	// Present [from+1, from+5], HOLE at from+6, present [from+7, from+10] —
	// an interior hole ABOVE the resume point, exactly the near-tip drop shape.
	for seq := from + 1; seq <= from+5; seq++ {
		seed(seq)
	}
	for seq := from + 7; seq <= from+10; seq++ {
		seed(seq)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush hole seed: %v", err)
	}

	// from explicit (skip the watermark read); to=0 → resolve the contiguous tip.
	// floorLedger is unused on this path (from!=0 skips the first-run seed) —
	// pass any valid non-zero value.
	start, last, err := chops.Cap67Range(ctx, addr, from+1, 0, 1)
	if err != nil {
		t.Fatalf("Cap67Range: %v", err)
	}
	if start != from+1 {
		t.Fatalf("start = %d, want %d", start, from+1)
	}
	if last != from+5 {
		t.Fatalf("Cap67Range last = %d, want %d — the derive MUST stall before the "+
			"hole at %d, not step past it (pre-fix MaxLedger would return >= %d and "+
			"permanently drop the hole ledger's movements)", last, from+5, from+6, from+10)
	}
}

// TestCap67Range_FirstRunClampsToTheLakesFirstLedger is the test nets'
// empty-archive proof: a FIRST run (no watermark row) floored
// at genesis against a lake that begins at ledger 2 — every net's lake,
// ledger 1 is never exported — must derive from 2, not idle forever.
//
// Starting at start = floorLedger = 1 and asking ContiguousWatermark
// for the tip from 1 fails: with min_present = 2 > from that is a boundary hole,
// so it answers 0, the caller's `last < start` guard reads "nothing to do",
// and the daemon re-runs that full-lake window-function scan every second
// without writing a row or a journal line. This is a
// real-ClickHouse test because the clamp's input IS what the lake reports
// as its first ledger: seed [2, 6], skip 1, and read the range back through
// the real watermark + min queries.
//
// Proven red: with resolveStart's lakeMin clamp removed, start = 1 and
// last = 0.
func TestCap67Range_FirstRunClampsToTheLakesFirstLedger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Precondition: a first run. Nothing in this suite writes the cap67
	// watermark, so from=0 takes the floor path rather than resuming.
	wm, err := chstore.Cap67MovementsWatermark(ctx, addr)
	if err != nil {
		t.Fatalf("Cap67MovementsWatermark: %v", err)
	}
	if wm != 0 {
		t.Fatalf("precondition: cap67 watermark = %d, want 0 (a first run); another test now writes it", wm)
	}

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	// The lake's own shape: ledgers 2..6 present, genesis (1) absent. The
	// low range is deliberate — the clamp is against the GLOBAL
	// min(ledger_seq), so these must be the lowest ledgers the suite seeds
	// (every other ClickHouse test seeds >= 1,000).
	for seq := uint32(2); seq <= 6; seq++ {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: time.Date(2027, 9, 1, 0, 0, 0, 0, time.UTC),
			LedgerHash: "aa02", PrevHash: "bb02", ProtocolVersion: 23, BucketListHash: "cc02",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush lake seed: %v", err)
	}

	lakeMin, err := chstore.LakeMinLedger(ctx, addr)
	if err != nil {
		t.Fatalf("LakeMinLedger: %v", err)
	}
	if lakeMin != 2 {
		t.Fatalf("LakeMinLedger = %d, want 2 (the seeded lake begins at 2; a lower value means another test seeded below it)", lakeMin)
	}

	// from=0 (resume from the watermark — none, so the floor), to=0 (the
	// contiguous tip), floor=1 (genesis: the test nets' -floor-ledger).
	start, last, err := chops.Cap67Range(ctx, addr, 0, 0, 1)
	if err != nil {
		t.Fatalf("Cap67Range: %v", err)
	}
	if start != 2 {
		t.Fatalf("Cap67Range start = %d, want 2 — a first run floored at genesis must clamp up to the "+
			"lake's first ledger; at 1 the contiguity gate reads a boundary hole and the daemon never derives", start)
	}
	if last < 6 {
		t.Fatalf("Cap67Range last = %d, want >= 6 — the seeded run [2,6] is contiguous, so the first run "+
			"must resolve a non-empty range (pre-fix: last = 0, `last < start`, idle forever)", last)
	}
}
