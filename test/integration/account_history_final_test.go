//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestClickHouseAccountHistoryServesNewestUnmergedVersion is the GH-1141
// regression: stellar.transactions and stellar.operations are
// ReplacingMergeTree(ingested_at), so a re-derive leaves the stale and the
// corrected row in separate parts until a background merge. The account
// listings' `LIMIT 1 BY` collapses the pair to ONE row but picks it by part
// order, not by version; only FINAL applies the engine's rule (highest
// ingested_at, and on a same-second tie the last insert), which is what
// /v1/tx/{hash} already serves. The three keys cover both part orders and the
// same-second tie, so no fixed part-order tie-break can pass them all.
func TestClickHouseAccountHistoryServesNewestUnmergedVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		account = "GTEST_GH1141_REDERIVE_ACCOUNT_AAAAAAAAAAAAAAAAAAAAA"
		seq     = uint32(6_420_001)
	)
	// Far below the throughput test's global close_time tip (see
	// account_activity_watermark_test.go).
	closeTime := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	older, newer := closeTime.Add(10*time.Second), closeTime.Add(20*time.Second)
	type version struct {
		ingestedAt time.Time
		fresh      bool
	}
	// Per tx_index: the version written to part 1, then to part 2.
	cases := [][2]version{
		{{older, false}, {newer, true}}, // re-derive lands later, higher version
		{{newer, true}, {older, false}}, // higher version sits in the EARLIER part
		{{older, false}, {older, true}}, // same-second re-derive: last insert wins
	}
	memo := func(fresh bool) string {
		if fresh {
			return "fresh-post-rederive"
		}
		return "stale-pre-rederive"
	}
	fee := func(fresh bool) int64 {
		if fresh {
			return 200
		}
		return 100
	}
	body := func(fresh bool) string {
		if fresh {
			return "ZnJlc2g="
		}
		return "c3RhbGU="
	}
	txHash := func(i int) string { return fmt.Sprintf("%064d", 1141_000+i) }

	raw := dialClickHouse(t, ctx, "stellar")
	for _, tbl := range []string{"stellar.transactions", "stellar.operations"} {
		if err := raw.Exec(ctx, "SYSTEM STOP MERGES "+tbl); err != nil {
			t.Fatalf("SYSTEM STOP MERGES %s: %v", tbl, err)
		}
		t.Cleanup(func() { _ = raw.Exec(context.Background(), "SYSTEM START MERGES "+tbl) })
	}

	// One INSERT per part per table → two un-merged parts each.
	for part := 0; part < 2; part++ {
		tb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.transactions
			(ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee,
			 operation_count, successful, result_code, memo_type, memo, ingested_at)`)
		if err != nil {
			t.Fatalf("prepare tx batch: %v", err)
		}
		ob, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.operations
			(ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr, ingested_at)`)
		if err != nil {
			t.Fatalf("prepare op batch: %v", err)
		}
		for i, c := range cases {
			v := c[part]
			if err := tb.Append(seq, closeTime, txHash(i), uint32(i), account, fee(v.fresh), int64(1000),
				uint16(1), uint8(1), int32(0), "MemoTypeMemoText", memo(v.fresh), v.ingestedAt); err != nil {
				t.Fatalf("append tx: %v", err)
			}
			if err := ob.Append(seq, closeTime, txHash(i), uint32(i), uint32(0), "OperationTypePayment",
				account, body(v.fresh), v.ingestedAt); err != nil {
				t.Fatalf("append op: %v", err)
			}
		}
		if err := tb.Send(); err != nil {
			t.Fatalf("send tx: %v", err)
		}
		if err := ob.Send(); err != nil {
			t.Fatalf("send op: %v", err)
		}
	}

	// Precondition: both versions of every key are physically present, or
	// this test proves nothing about FINAL.
	for _, q := range []string{
		`SELECT count() FROM stellar.transactions WHERE ledger_seq = ? GROUP BY ledger_seq, tx_index`,
		`SELECT count() FROM stellar.operations WHERE ledger_seq = ? GROUP BY ledger_seq, tx_index, op_index`,
	} {
		rows, err := raw.Query(ctx, q, seq)
		if err != nil {
			t.Fatalf("count versions: %v", err)
		}
		keys := 0
		for rows.Next() {
			var n uint64
			if err := rows.Scan(&n); err != nil {
				t.Fatalf("scan versions: %v", err)
			}
			if n != 2 {
				t.Fatalf("%s: a key has %d row(s), want 2 un-merged versions", q, n)
			}
			keys++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("count versions rows: %v", err)
		}
		_ = rows.Close()
		if keys != len(cases) {
			t.Fatalf("%s: %d key(s), want %d", q, keys, len(cases))
		}
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	txs, err := er.AccountTransactions(ctx, account, 50, chstore.ExplorerCursor{})
	if err != nil {
		t.Fatalf("AccountTransactions: %v", err)
	}
	if len(txs) != len(cases) {
		t.Fatalf("AccountTransactions returned %d rows, want %d (one per key): %+v", len(txs), len(cases), txs)
	}
	for _, tx := range txs {
		if tx.Memo != memo(true) || tx.FeeCharged != fee(true) {
			t.Errorf("AccountTransactions tx_index=%d served memo=%q fee=%d, want the newest version memo=%q fee=%d",
				tx.TxIndex, tx.Memo, tx.FeeCharged, memo(true), fee(true))
		}
	}

	ops, err := er.AccountOperations(ctx, account, 50, chstore.ExplorerCursor{})
	if err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	if len(ops) != len(cases) {
		t.Fatalf("AccountOperations returned %d rows, want %d (one per key): %+v", len(ops), len(cases), ops)
	}
	for _, op := range ops {
		if op.BodyXDR != body(true) {
			t.Errorf("AccountOperations tx_index=%d served body_xdr=%q, want the newest version %q",
				op.TxIndex, op.BodyXDR, body(true))
		}
	}
}
