//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

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

// TestVerifyContiguity_AutoECFloorGatesHoleBelowOldConstant is the GH-1092
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
