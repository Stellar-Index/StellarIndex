//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestClickHouseAccountActivityWatermarkBoundedOps is the live-ClickHouse
// proof for the #31 activity-watermark bound on AccountOperations.
//
// Scenario (the measured live pathology): a long-idle account whose rows all
// sit far below the tip — the reader's reverse primary-key resolves used to
// walk granules from the tip back to the account's last activity (~4s live
// for a 46d-idle account, 2026-08-24). The fix bounds each arm with
// `ledger_seq <= max(account_activity.last_ledger)`.
//
// What this test proves, in order of importance:
//
//  1. DATA-HIDING invariant: the bounded read returns EXACTLY the rows the
//     account has — including an op where the account is only a NON-SOURCE
//     participant at a ledger ABOVE its last SOURCED activity (the
//     Soroban/SAC "token movement postdates the classic account's own
//     activity" shape). A watermark tracking only source_account would bound
//     below that row and silently hide it. Red-proof: force the reader's
//     bound to min(last_ledger) instead of max(...) and this test fails on
//     the missing participant row.
//  2. The watermark itself is the max over ALL roles (participant ledger,
//     not the higher of the sourced ledgers).
//  3. The bound actually prunes: EXPLAIN ESTIMATE over the reverse
//     primary-key scan shape with the bound reads strictly fewer
//     stellar.operations rows than without it (the decoy tip partition is
//     pruned) — see the inline note on why the scan shape, not the IN-arm,
//     is what demonstrates the mechanism.
func TestClickHouseAccountActivityWatermarkBoundedOps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		idle  = "GTEST_WM31_IDLE_ACCOUNT_AAAAAAAAAAAAAAAAAAAAAAAAA"
		other = "GTEST_WM31_OTHER_SOURCE_AAAAAAAAAAAAAAAAAAAAAAAAA"
		decoy = "GTEST_WM31_DECOY_TIP_AAAAAAAAAAAAAAAAAAAAAAAAAAAA"

		sourcedLo   = uint32(6_210_001)  // idle's sourced ops: 3 consecutive ledgers from here
		participant = uint32(6_310_007)  // idle is a NON-SOURCE participant here (postdates sourced)
		tip         = uint32(82_310_001) // decoy activity in a far-higher partition (the "tip")
	)
	closeAt := func(seq uint32) time.Time {
		return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq-sourcedLo) * time.Second)
	}
	hash := func(seq uint32) string { return fmt.Sprintf("%064d", seq) }

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	addLedger := func(seq uint32, txSource, opSource string, participants []string) {
		ext := chstore.LedgerExtract{
			Ledger: chstore.LedgerRow{
				LedgerSeq: seq, CloseTime: closeAt(seq), LedgerHash: "aa", PrevHash: "bb",
				ProtocolVersion: 22, TxCount: 1, OpCount: 1,
			},
			Txs: []chstore.TransactionRow{{
				LedgerSeq: seq, CloseTime: closeAt(seq), TxHash: hash(seq),
				TxIndex: 0, SourceAccount: txSource, Successful: 1,
			}},
			Ops: []chstore.OperationRow{{
				LedgerSeq: seq, CloseTime: closeAt(seq), TxHash: hash(seq),
				TxIndex: 0, OpIndex: 0, OpType: "OperationTypePayment", SourceAccount: opSource, BodyXDR: "Ym9keQ==",
			}},
		}
		for _, p := range participants {
			ext.Participants = append(ext.Participants, chstore.OperationParticipantRow{
				Account: p, LedgerSeq: seq, CloseTime: closeAt(seq), TxHash: hash(seq), TxIndex: 0, OpIndex: 0,
			})
		}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}

	// The idle account's own sourced activity, far below the tip.
	for i := uint32(0); i < 3; i++ {
		addLedger(sourcedLo+i, idle, idle, nil)
	}
	// A LATER op sourced by someone else in which idle only participates —
	// the row a sourced-only watermark would hide.
	addLedger(participant, other, other, []string{idle})
	// Decoy tip activity in a far-higher partition: the granules the
	// unbounded resolve walks through and the bounded one must skip.
	for i := uint32(0); i < 3; i++ {
		addLedger(tip+i, decoy, decoy, nil)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	raw := dialClickHouse(t, ctx, "stellar")

	// (2) The MV-maintained watermark must be the max over ALL roles: the
	// participant ledger, not the higher sourced ledger.
	var wm uint32
	if err := raw.QueryRow(ctx,
		`SELECT max(last_ledger) FROM stellar.account_activity WHERE account_id = ?`, idle).Scan(&wm); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if wm != participant {
		t.Fatalf("watermark = %d, want %d — account_activity must cover EVERY role the ops query "+
			"filters on (a source-only watermark = %d would bound below the participant row and HIDE it)",
			wm, participant, sourcedLo+2)
	}

	// (1) Bounded read loses nothing: newest-first, participant row first,
	// then the three sourced rows. The reader takes the bounded path here
	// (the watermark row exists — asserted above), so equality against the
	// seeded ground truth IS bounded-vs-unbounded equality.
	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	rows, err := er.AccountOperations(ctx, idle, 50, chstore.ExplorerCursor{})
	if err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	wantSeqs := []uint32{participant, sourcedLo + 2, sourcedLo + 1, sourcedLo}
	if len(rows) != len(wantSeqs) {
		got := make([]uint32, len(rows))
		for i, r := range rows {
			got[i] = r.Seq
		}
		t.Fatalf("AccountOperations returned %d rows %v, want %d %v — a missing row means the "+
			"watermark bound HID history (the data-hiding failure the #31 invariant forbids)",
			len(rows), got, len(wantSeqs), wantSeqs)
	}
	for i, want := range wantSeqs {
		if rows[i].Seq != want {
			t.Fatalf("row %d at ledger %d, want %d (newest first)", i, rows[i].Seq, want)
		}
	}

	// Pagination across the bound: cursor below the participant row must
	// serve the sourced rows, nothing lost at the seam.
	page2, err := er.AccountOperations(ctx, idle, 50, chstore.ExplorerCursor{Ledger: participant, A: 0, B: 0})
	if err != nil {
		t.Fatalf("AccountOperations page 2: %v", err)
	}
	if len(page2) != 3 || page2[0].Seq != sourcedLo+2 {
		t.Fatalf("cursor page returned %d rows (first seq %v), want the 3 sourced rows from %d",
			len(page2), page2, sourcedLo+2)
	}

	// (3) The bound prunes: EXPLAIN ESTIMATE of the reverse primary-key
	// scan must read strictly fewer stellar.operations rows WITH the bound
	// (the decoy tip partition — intDiv 82 vs the bound's 6 — cannot be
	// pruned without it). The scan SHAPE, not the full IN-arm: on a toy
	// dataset ClickHouse's set-based index analysis resolves the IN
	// subquery to exact granules either way, which is precisely the
	// heuristic that did NOT save the 10.6B-row live table (~4s tip walk,
	// 2026-08-24) — the watermark's value is DETERMINISTIC partition +
	// primary-key range pruning, independent of set-analysis heuristics
	// and their size caps, and that is the mechanism asserted here.
	armSQL := func(bound string) string {
		where := ""
		if bound != "" {
			where = ` WHERE ` + bound
		}
		return `EXPLAIN ESTIMATE SELECT ledger_seq, tx_index, op_index FROM stellar.operations` + where + `
			ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC LIMIT 50`
	}
	opsRowsEstimate := func(sql string) uint64 {
		res, err := raw.Query(ctx, sql)
		if err != nil {
			t.Fatalf("EXPLAIN ESTIMATE: %v", err)
		}
		defer func() { _ = res.Close() }()
		var total uint64
		for res.Next() {
			var db, table string
			var parts, nrows, marks uint64
			if err := res.Scan(&db, &table, &parts, &nrows, &marks); err != nil {
				t.Fatalf("scan EXPLAIN ESTIMATE: %v", err)
			}
			if table == "operations" {
				total += nrows
			}
		}
		if err := res.Err(); err != nil {
			t.Fatalf("EXPLAIN ESTIMATE rows: %v", err)
		}
		return total
	}
	unbounded := opsRowsEstimate(armSQL(""))
	bounded := opsRowsEstimate(armSQL(fmt.Sprintf("ledger_seq <= %d", wm)))
	if bounded >= unbounded {
		t.Fatalf("bounded arm reads %d operations rows, unbounded %d — the watermark bound must "+
			"prune the tip partitions (granule-bounded scan is the whole point of #31)", bounded, unbounded)
	}
}
