//go:build integration

package integration_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/stellar/go-stellar-sdk/historyarchive"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestClickHouseContractEventsRMTDedup is the live-ClickHouse proof that
// ContractEventsRecent (Go-side adjacent-row dedup — a SQL LIMIT 1 BY disables
// reverse read-in-order and costs 100× on busy contracts) and EventsByTx (FINAL) must serve each contract
// event EXACTLY ONCE even while
// stellar.contract_events — a ReplacingMergeTree — holds an un-merged duplicate
// part (the legitimate post-heal / ch-rebuild / partial-flush-retry state).
//
// Determinism: the two same-key inserts create two parts, and a background merge
// would collapse them on its own (making a non-deduping reader accidentally pass).
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

// TestRunCensusDay_PrunesByCloseTimeAndSwapsPartition runs the census rollup
// against a real server on the certified Tier-1 schema and proves the two
// things its unit tests cannot:
//
//  1. the day window WHERE close_time >= d AND close_time < d+1 is served by
//     a minmax skip index (idx_ce_close_time) that DROPS granules outside
//     the day — contract_events is partitioned and sorted by ledger_seq, so
//     before the index existed the 30-minute rollup read every granule of
//     the table per run;
//  2. RunCensusDay's private-staging CREATE / INSERT / REPLACE PARTITION /
//     DROP sequence executes and lands exact per-contract counts for the
//     day and nothing from the neighbouring day.
func TestRunCensusDay_PrunesByCloseTimeAndSwapsPartition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const (
		baseLedger = uint32(90_000_001)
		contractA  = "CTEST_CENSUS_A_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		contractB  = "CTEST_CENSUS_B_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	)
	day := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	inDay := day.Add(6 * time.Hour)
	nextDay := day.Add(24 * time.Hour).Add(time.Hour)

	event := func(seq uint32, at time.Time, contract string, eventIndex uint32) chstore.ContractEventRow {
		return chstore.ContractEventRow{
			LedgerSeq: seq, CloseTime: at, TxHash: fmt.Sprintf("%064x", seq),
			OpIndex: 0, EventIndex: eventIndex, ContractID: contract, EventType: "contract",
			TopicCount: 0, TopicsXDR: []string{}, DataXDR: "", OpArgsXDR: []string{}, InSuccessfulCall: 1,
		}
	}
	// Two separate flushes → two parts, so the neighbouring day is a granule
	// of its own that the skip index can drop. Merges are paused so the
	// server cannot fold both days into one granule underneath the EXPLAIN.
	if err := conn.Exec(ctx, "SYSTEM STOP MERGES stellar.contract_events"); err != nil {
		t.Fatalf("stop merges: %v", err)
	}
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "SYSTEM START MERGES stellar.contract_events") })
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	for _, ext := range []chstore.LedgerExtract{
		{
			Ledger: chstore.LedgerRow{LedgerSeq: baseLedger, CloseTime: inDay, ProtocolVersion: 22, SorobanEventCount: 3},
			Events: []chstore.ContractEventRow{
				event(baseLedger, inDay, contractA, 0),
				event(baseLedger, inDay, contractA, 1),
				event(baseLedger, inDay, contractB, 2),
			},
		},
		{
			Ledger: chstore.LedgerRow{LedgerSeq: baseLedger + 1, CloseTime: nextDay, ProtocolVersion: 22, SorobanEventCount: 1},
			Events: []chstore.ContractEventRow{event(baseLedger+1, nextDay, contractA, 0)},
		},
	} {
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add: %v", err)
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("sink flush: %v", err)
		}
	}

	// (1) the index is declared on the live table …
	var idxType, idxExpr string
	if err := conn.QueryRow(ctx, `SELECT type, expr FROM system.data_skipping_indices
		WHERE database = 'stellar' AND table = 'contract_events' AND name = 'idx_ce_close_time'`).
		Scan(&idxType, &idxExpr); err != nil {
		t.Fatalf("stellar.contract_events has no skip index idx_ce_close_time — the census day window full-scans: %v", err)
	}
	if idxType != "minmax" || idxExpr != "close_time" {
		t.Fatalf("idx_ce_close_time = %s(%s), want minmax(close_time)", idxType, idxExpr)
	}
	// … and the planner actually drops granules with it for the rollup's own
	// predicate shape.
	plan := explain(t, ctx, conn, fmt.Sprintf(`EXPLAIN indexes = 1 SELECT count() FROM stellar.contract_events
		WHERE close_time >= toDateTime('%s', 'UTC') AND close_time < toDateTime('%s', 'UTC')`,
		day.Format("2006-01-02 15:04:05"), day.Add(24*time.Hour).Format("2006-01-02 15:04:05")))
	selected, initial := skipIndexGranules(t, plan, "idx_ce_close_time")
	if selected >= initial {
		t.Fatalf("idx_ce_close_time dropped no granules (%d/%d) — the neighbouring day's part should have been pruned:\n%s", selected, initial, plan)
	}

	// (2) the rollup lands exact counts for the day and only the day.
	if err := chstore.RunCensusDay(ctx, addr, day, false, t.Logf); err != nil {
		t.Fatalf("RunCensusDay: %v", err)
	}
	rows, err := conn.Query(ctx, `SELECT contract_id, events, last_ledger FROM stellar.contracts_census_daily
		WHERE day = ? AND contract_id IN (?, ?) ORDER BY contract_id`, day, contractA, contractB)
	if err != nil {
		t.Fatalf("read census: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string][2]uint64{}
	for rows.Next() {
		var id string
		var events uint64
		var last uint32
		if err := rows.Scan(&id, &events, &last); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = [2]uint64{events, uint64(last)}
	}
	want := map[string][2]uint64{contractA: {2, uint64(baseLedger)}, contractB: {1, uint64(baseLedger)}}
	if len(got) != len(want) {
		t.Fatalf("census rows = %v, want %v", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("census[%s] = (events %d, last_ledger %d), want (events %d, last_ledger %d)", id, got[id][0], got[id][1], w[0], w[1])
		}
	}

	// (3) staging is per-run and private: the run's own table is dropped on
	// exit, and the Tier-1 schema declares no shared twin for anything to
	// leave behind.
	var staging []string
	srows, err := conn.Query(ctx, `SELECT name FROM system.tables
		WHERE database = 'stellar' AND name LIKE 'contracts_census_daily_staging%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list staging tables: %v", err)
	}
	defer func() { _ = srows.Close() }()
	for srows.Next() {
		var n string
		if err := srows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		staging = append(staging, n)
	}
	if len(staging) != 0 {
		t.Fatalf("census staging tables left after the run: %v, want none (the shared stellar.contracts_census_daily_staging is dead DDL; the private one is dropped)", staging)
	}
}

// explain returns EXPLAIN output as one newline-joined string.
func explain(t *testing.T, ctx context.Context, conn driver.Conn, q string) string {
	t.Helper()
	rows, err := conn.Query(ctx, q)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatalf("explain scan: %v", err)
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

// skipIndexGranules finds the `Skip` block for the named index in an
// `EXPLAIN indexes = 1` plan and returns its "Granules: selected/initial".
func skipIndexGranules(t *testing.T, plan, name string) (selected, initial int) {
	t.Helper()
	lines := strings.Split(plan, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "Name: "+name {
			continue
		}
		for _, m := range lines[i+1:] {
			m = strings.TrimSpace(m)
			if !strings.HasPrefix(m, "Granules: ") {
				continue
			}
			parts := strings.SplitN(strings.TrimPrefix(m, "Granules: "), "/", 2)
			if len(parts) != 2 {
				break
			}
			a, errA := strconv.Atoi(parts[0])
			b, errB := strconv.Atoi(parts[1])
			if errA != nil || errB != nil {
				break
			}
			return a, b
		}
		break
	}
	t.Fatalf("plan does not consult skip index %s:\n%s", name, plan)
	return 0, 0
}

// TestContiguousThroughDay_StopsAtHole runs the census walk bound's SQL on a
// real server: a hole on day D pins the bound to D (so D+1 is not computed
// and D stays the resume point), and healing the hole releases it.
func TestContiguousThroughDay_StopsAtHole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Isolated range above every other suite ledger (the watermark test uses
	// 215M) with the latest close times, so this test owns both the global
	// ledger max and the "last ledger before the day" lookup.
	const base = uint32(300_000_000)
	d := time.Date(2040, 3, 10, 0, 0, 0, 0, time.UTC)
	next := d.Add(24 * time.Hour)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	// Owning the global max is only safe while this test runs: left behind, these
	// rows sit above sdex_orderbook_lake_hole_test's range, which must be the tip.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		conn := dialClickHouse(t, cctx, "stellar")
		if err := conn.Exec(cctx, fmt.Sprintf(`ALTER TABLE stellar.ledgers DELETE
			WHERE ledger_seq BETWEEN %d AND %d SETTINGS mutations_sync = 2`, base, base+4)); err != nil {
			t.Errorf("purge census fixture ledgers: %v", err)
		}
	})
	seed := func(seq uint32, at time.Time) {
		t.Helper()
		if err := sink.Add(ctx, chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: at, LedgerHash: "aa00", PrevHash: "bb00", ProtocolVersion: 22,
			BucketListHash: "cc00", TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("flush ledger %d: %v", seq, err)
		}
	}
	seed(base, d.Add(-time.Hour))
	seed(base+1, d.Add(time.Hour))
	// base+2 (day D, 02:00) is the LiveSink hole.
	seed(base+3, d.Add(3*time.Hour))
	seed(base+4, next.Add(time.Hour))

	got, ok, err := chstore.ContiguousThroughDay(ctx, addr, d)
	if err != nil || !ok || !got.Equal(d) {
		t.Fatalf("hole on %s: ContiguousThroughDay = (%s, %v, %v), want (%s, true, nil)", d, got, ok, err, d)
	}

	// Nothing at or after D+2, and ledger base+5 is not there yet: nothing
	// is contiguous from the start of that day.
	if _, ok, err := chstore.ContiguousThroughDay(ctx, addr, next.Add(24*time.Hour)); err != nil || ok {
		t.Fatalf("day past the lake tip: ok=%v err=%v, want ok=false", ok, err)
	}

	seed(base+2, d.Add(2*time.Hour)) // ch-live-catchup heals the hole
	got, ok, err = chstore.ContiguousThroughDay(ctx, addr, d)
	if err != nil || !ok || !got.Equal(next) {
		t.Fatalf("healed: ContiguousThroughDay = (%s, %v, %v), want (%s, true, nil)", got, ok, err, next)
	}
}

// TestRunCensusDay_RefusesShrink runs the shrink check's SQL on a real
// server: a live partition of 2 contracts is not replaced by a recompute of
// a day the lake holds no events for, unless shrinkOK.
func TestRunCensusDay_RefusesShrink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	day := time.Date(2040, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := conn.Exec(ctx, `INSERT INTO stellar.contracts_census_daily (day, contract_id, events, last_ledger, last_seen)
		VALUES (?, 'CTEST_SHRINK_A', 7, 1, ?), (?, 'CTEST_SHRINK_B', 3, 1, ?)`, day, day, day, day); err != nil {
		t.Fatalf("seed live partition: %v", err)
	}
	liveRows := func() uint64 {
		t.Helper()
		var n uint64
		if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.contracts_census_daily WHERE day = ?`, day).Scan(&n); err != nil {
			t.Fatalf("count live partition: %v", err)
		}
		return n
	}

	err := chstore.RunCensusDay(ctx, addr, day, false, t.Logf)
	if !errors.Is(err, chstore.ErrCensusShrink) {
		t.Fatalf("empty recompute over a 2-row live day returned %v, want ErrCensusShrink", err)
	}
	if n := liveRows(); n != 2 {
		t.Fatalf("live partition has %d row(s) after a refused shrink, want the original 2", n)
	}

	if err := chstore.RunCensusDay(ctx, addr, day, true, t.Logf); err != nil {
		t.Fatalf("shrinkOK recompute: %v", err)
	}
	if n := liveRows(); n != 0 {
		t.Fatalf("live partition has %d row(s) after a -shrink-ok recompute of an empty day, want 0", n)
	}
}

// The instance backfill reads ledger_entry_changes, a
// ReplacingMergeTree(ingested_at), and a corrected re-ingest leaves the stale
// part beside the fix until a merge. Without FINAL both rows ride one INSERT
// into contract_instance_changes, tie on its DEFAULT now() version, and the
// stale wasm_hash can be the one code-history serves. The stale part is
// written LAST so an unmerged read hands it to the target last.
func TestContractInstanceBackfill_ReadsCorrectedSourceRowOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const ledger = uint32(72_719_001)
	var cidRaw xdr.Hash
	copy(cidRaw[:], []byte("gh719-instance-backfill-final-c1"))
	cid := xdr.ContractId(cidRaw)
	contractHash := hex.EncodeToString(cidRaw[:])
	stale, corrected := t356Hash(0x20), t356Hash(0x21)
	instKey, correctedEntry := t356InstanceKeyAndEntry(t, cid, corrected)
	_, staleEntry := t356InstanceKeyAndEntry(t, cid, stale)

	row := chstore.LedgerEntryChangeRow{
		LedgerSeq: ledger, CloseTime: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC), TxHash: "gh719-upgrade",
		OpIndex: 0, ChangeIndex: 0, IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
		KeyXDR: instKey, EntryXDR: correctedEntry,
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{row}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	// The same source key, one hour OLDER by version: the stale row a
	// merge would discard, still sitting in its own part.
	if err := conn.Exec(ctx, `INSERT INTO stellar.ledger_entry_changes
		SELECT * REPLACE (? AS entry_xdr, ingested_at - INTERVAL 1 HOUR AS ingested_at)
		FROM stellar.ledger_entry_changes WHERE ledger_seq = ? AND tx_hash = ?`,
		staleEntry, ledger, row.TxHash); err != nil {
		t.Fatalf("insert stale part: %v", err)
	}
	var parts uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.ledger_entry_changes
		WHERE ledger_seq = ? AND tx_hash = ?`, ledger, row.TxHash).Scan(&parts); err != nil || parts != 2 {
		t.Fatalf("unmerged source rows = %d (%v), want 2 (the fix and its stale twin)", parts, err)
	}

	// Drop what the MV wrote at insert time, so the target holds only the
	// backfill's output.
	if err := conn.Exec(ctx, `ALTER TABLE stellar.contract_instance_changes DELETE
		WHERE contract_hash = ? SETTINGS mutations_sync = 2`, contractHash); err != nil {
		t.Fatalf("clear MV rows: %v", err)
	}
	if err := chstore.BackfillContractInstanceChanges(ctx, addr, ledger, ledger, 10, t.Logf); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var got []string
	q, err := conn.Query(ctx, `SELECT wasm_hash FROM stellar.contract_instance_changes FINAL
		WHERE contract_hash = ? AND ledger_seq = ?`, contractHash, ledger)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	for q.Next() {
		var h string
		if err := q.Scan(&h); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, h)
	}
	_ = q.Close()
	want := hex.EncodeToString(corrected[:])
	if len(got) != 1 || got[0] != want {
		t.Fatalf("backfilled wasm_hash = %v, want exactly the corrected [%s] (stale %s must not survive)",
			got, want, hex.EncodeToString(stale[:]))
	}
}

// contract_instance_changes keyed (contract_hash, ledger_seq,
// change_index), and change_index restarts per TRANSACTION. Two transactions
// writing one contract's instance in the same ledger — both at change_index 0
// — collapsed to one row on merge, so a same-ledger upgrade vanished from the
// code history and the survivor was the last INSERTED, not the ledger-final
// write. This drives the shipped DDL (tier1 + the r1 migration's v2), its MV,
// the backfill INSERT and both indexed reader queries on a real ClickHouse.
//
// Red against the old key: after OPTIMIZE … FINAL only one ledger-L row
// survives, so the count assertions fail and the history loses a version.

const (
	t356Ledger0 = uint32(72_356_001) // deploy: executable h0
	t356Ledger  = uint32(72_356_050) // tx A installs h1, then tx B installs h2
)

func t356Hash(b byte) xdr.Hash {
	var h xdr.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

func t356InstanceKeyAndEntry(t *testing.T, cid xdr.ContractId, wasm xdr.Hash) (string, string) {
	t.Helper()
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	key := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   addr,
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	inst := xdr.ScContractInstance{Executable: xdr.ContractExecutable{
		Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &wasm,
	}}
	entry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   addr,
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &inst},
		},
	}}
	k, err := xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal instance key: %v", err)
	}
	e, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal instance entry: %v", err)
	}
	return k, e
}

func t356CodeKeyAndEntry(t *testing.T, wasm xdr.Hash) (string, string) {
	t.Helper()
	var key xdr.LedgerKey
	if err := key.SetContractCode(wasm); err != nil {
		t.Fatalf("contract_code key: %v", err)
	}
	entry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type:         xdr.LedgerEntryTypeContractCode,
		ContractCode: &xdr.ContractCodeEntry{Hash: wasm, Code: []byte("\x00asm\x01\x00\x00\x00")},
	}}
	k, err := xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal code key: %v", err)
	}
	e, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal code entry: %v", err)
	}
	return k, e
}

// applyDeployStatements executes every statement of one deploy/clickhouse
// artifact — for the migration, that is its Step-1 CREATEs only; the later
// steps are operator-run comments.
func applyDeployStatements(t *testing.T, ctx context.Context, conn driver.Conn, name string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "clickhouse", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	for _, s := range splitSQLStatements(string(raw)) {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %.80q: %v", name, s, err)
		}
	}
}

func t356CountRows(t *testing.T, ctx context.Context, conn driver.Conn, table, contractHash string, ledger uint32) uint64 {
	t.Helper()
	if err := conn.Exec(ctx, "OPTIMIZE TABLE stellar."+table+" FINAL"); err != nil {
		t.Fatalf("optimize %s: %v", table, err)
	}
	var n uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM stellar."+table+
		" FINAL WHERE contract_hash = ? AND ledger_seq = ?", contractHash, ledger).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestContractInstanceChanges_SameLedgerTransactionsBothSurvive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	applyDeployStatements(t, ctx, conn, "contract_instance_changes_tx_key.sql")
	t.Cleanup(func() {
		_ = conn.Exec(context.Background(), "DROP VIEW IF EXISTS stellar.contract_instance_changes_v2_mv")
		_ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS stellar.contract_instance_changes_v2")
	})

	var cidRaw xdr.Hash
	copy(cidRaw[:], []byte("t356-two-tx-same-ledger-contract"))
	cid := xdr.ContractId(cidRaw)
	contractHash := hex.EncodeToString(cidRaw[:])
	contractStrkey, err := strkey.Encode(strkey.VersionByteContract, cidRaw[:])
	if err != nil {
		t.Fatalf("strkey: %v", err)
	}
	h0, h1, h2 := t356Hash(0x10), t356Hash(0x11), t356Hash(0x12)
	instKey, entry0 := t356InstanceKeyAndEntry(t, cid, h0)
	_, entry1 := t356InstanceKeyAndEntry(t, cid, h1)
	_, entry2 := t356InstanceKeyAndEntry(t, cid, h2)
	codeKey, codeEntry := t356CodeKeyAndEntry(t, h2)
	closeTime := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: t356Ledger0, CloseTime: closeTime, TxHash: "t356-deploy", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 3, ChangeType: "created", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry0,
		},
		{
			LedgerSeq: t356Ledger0, CloseTime: closeTime, TxHash: "t356-deploy", OpIndex: 0, ChangeIndex: 1,
			IntraLedgerSeq: 4, ChangeType: "created", EntryType: "contract_code", KeyXDR: codeKey, EntryXDR: codeEntry,
		},
		// Tx B is ledger-final (intra 9) and is handed to the writer FIRST,
		// so an insertion-order survivor would be tx A's h1.
		{
			LedgerSeq: t356Ledger, CloseTime: closeTime, TxHash: "t356-tx-b", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 9, ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry2,
		},
		{
			LedgerSeq: t356Ledger, CloseTime: closeTime, TxHash: "t356-tx-a", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 4, ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry1,
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	// The MVs (canonical and the migration's v2) keep both same-ledger writes.
	for _, table := range []string{chstore.ContractInstanceChangesTable, chstore.ContractInstanceChangesV2Table} {
		if n := t356CountRows(t, ctx, conn, table, contractHash, t356Ledger); n != 2 {
			t.Fatalf("%s via MV: %d rows at ledger %d, want 2 (one per transaction)", table, n, t356Ledger)
		}
	}

	// The backfill INSERT, into the migration's target, re-derives the same.
	if err := conn.Exec(ctx, "TRUNCATE TABLE stellar.contract_instance_changes_v2"); err != nil {
		t.Fatalf("truncate v2: %v", err)
	}
	if err := chstore.BackfillContractInstanceChangesInto(ctx, addr, chstore.ContractInstanceChangesV2Table,
		t356Ledger0, t356Ledger, 1000, t.Logf); err != nil {
		t.Fatalf("backfill v2: %v", err)
	}
	if n := t356CountRows(t, ctx, conn, chstore.ContractInstanceChangesV2Table, contractHash, t356Ledger); n != 2 {
		t.Fatalf("backfill: %d rows at ledger %d, want 2", n, t356Ledger)
	}
	var ils []uint32
	var txs []string
	q, err := conn.Query(ctx, `SELECT intra_ledger_seq, tx_hash FROM stellar.contract_instance_changes_v2 FINAL
		WHERE contract_hash = ? AND ledger_seq = ? ORDER BY intra_ledger_seq`, contractHash, t356Ledger)
	if err != nil {
		t.Fatalf("read v2: %v", err)
	}
	for q.Next() {
		var s uint32
		var h string
		if err := q.Scan(&s, &h); err != nil {
			t.Fatalf("scan v2: %v", err)
		}
		ils, txs = append(ils, s), append(txs, h)
	}
	_ = q.Close()
	if len(ils) != 2 || ils[0] != 4 || ils[1] != 9 || txs[0] != "t356-tx-a" || txs[1] != "t356-tx-b" {
		t.Fatalf("backfilled (intra_ledger_seq, tx_hash) = %v %v, want [4 9] [t356-tx-a t356-tx-b]", ils, txs)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	hist, err := er.ContractCodeHistory(ctx, contractStrkey)
	if err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	want := []string{hex.EncodeToString(h0[:]), hex.EncodeToString(h1[:]), hex.EncodeToString(h2[:])}
	if len(hist) != len(want) {
		t.Fatalf("code history = %+v, want hashes %v (the same-ledger h1 must not vanish)", hist, want)
	}
	for i, v := range hist {
		if v.WasmHash != want[i] {
			t.Fatalf("code history[%d] = %s, want %s (ledger-final h2 last): %+v", i, v.WasmHash, want[i], hist)
		}
	}

	info, err := er.ContractWasm(ctx, contractStrkey)
	if err != nil {
		t.Fatalf("ContractWasm: %v", err)
	}
	if info.WasmHash != want[2] {
		t.Fatalf("ContractWasm hash = %s, want the ledger-final %s", info.WasmHash, want[2])
	}
}

// The genesis watermark is written by Set...Watermark (ordering-guarded) and
// consumed by ContractCodeHistory: an index miss skips the ledger_entry_changes
// scan only under a covering mark. Runs the real INSERT and SELECT.
func TestContractInstanceGenesisWatermark_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	const name = chstore.ContractInstanceChangesTable
	clearMark := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		if err := conn.Exec(cctx, `ALTER TABLE stellar.entry_history_watermark DELETE WHERE name = ? SETTINGS mutations_sync = 2`, name); err != nil {
			t.Fatalf("clear mark: %v", err)
		}
	}
	clearMark()
	t.Cleanup(clearMark)

	const ledger = uint32(72_720_001)
	var cidRaw xdr.Hash
	copy(cidRaw[:], []byte("inv2188-watermark-roundtrip-c001"))
	cid := xdr.ContractId(cidRaw)
	strk, err := strkey.Encode(strkey.VersionByteContract, cidRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	instKey, entry := t356InstanceKeyAndEntry(t, cid, t356Hash(0x31))
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{{
		LedgerSeq: ledger, CloseTime: time.Date(2025, 4, 2, 0, 0, 0, 0, time.UTC), TxHash: "inv2188-tx",
		IntraLedgerSeq: 1, ChangeType: "created", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry,
	}}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	// Leave the index non-empty (so it is "available") but without this
	// contract: an index miss the writer's mark alone can authorise.
	if err := conn.Exec(ctx, `ALTER TABLE stellar.contract_instance_changes DELETE WHERE contract_hash = ? SETTINGS mutations_sync = 2`,
		hex.EncodeToString(cidRaw[:])); err != nil {
		t.Fatalf("clear MV row: %v", err)
	}
	other, h33 := t356Hash(0x32), t356Hash(0x33)
	if err := conn.Exec(ctx, `INSERT INTO stellar.contract_instance_changes
		(contract_hash, ledger_seq, tx_hash, change_index, intra_ledger_seq, close_time, is_sac, wasm_hash)
		VALUES (?, 1, 'inv2188-other', 0, 1, now(), 0, ?)`, hex.EncodeToString(h33[:]), hex.EncodeToString(other[:])); err != nil {
		t.Fatalf("seed index row: %v", err)
	}

	history := func() int {
		er, err := chstore.NewExplorerReader(ctx, addr)
		if err != nil {
			t.Fatalf("NewExplorerReader: %v", err)
		}
		defer func() { _ = er.Close() }()
		h, err := er.ContractCodeHistory(ctx, strk)
		if err != nil {
			t.Fatalf("ContractCodeHistory: %v", err)
		}
		return len(h)
	}
	if n := history(); n != 1 {
		t.Fatalf("no mark: history = %d versions, want 1 (the scan must run)", n)
	}
	if err := chstore.SetContractInstanceChangesGenesisWatermark(ctx, addr, name, 500); err != nil {
		t.Fatal(err)
	}
	if n := history(); n != 0 {
		t.Fatalf("covering mark: history = %d versions, want 0 (scan skipped)", n)
	}
	// A later, lower mark must not replace the higher one.
	if err := chstore.SetContractInstanceChangesGenesisWatermark(ctx, addr, name, 300); err != nil {
		t.Fatal(err)
	}
	var rows uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.entry_history_watermark WHERE name = ?`, name).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("mark rows = %d (%v), want 1: a lower thru must record nothing", rows, err)
	}
}

// The mark is gated on the view's identity and age from system.tables; run
// that lookup for real against a table with a view and one without.
func TestContractInstanceGenesisWatermark_StartState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	st, err := chstore.ReadInstanceBackfillStart(ctx, addr, chstore.ContractInstanceChangesTable)
	if err != nil {
		t.Fatalf("ReadInstanceBackfillStart: %v", err)
	}
	if !st.MVExists || st.MVUUID == "" || st.MVModified.IsZero() || st.MVAge < 0 {
		t.Fatalf("start state = %+v, want the canonical view's identity and age", st)
	}
	again, err := chstore.ReadInstanceBackfillStart(ctx, addr, chstore.ContractInstanceChangesTable)
	if err != nil || again.MVUUID != st.MVUUID || !again.MVModified.Equal(st.MVModified) {
		t.Fatalf("re-read = %+v (%v), want the same view identity as %+v", again, err, st)
	}
	v2, err := chstore.ReadInstanceBackfillStart(ctx, addr, chstore.ContractInstanceChangesV2Table)
	if err != nil || v2.MVExists {
		t.Fatalf("v2 start state = %+v (%v), want no view", v2, err)
	}
}

// instanceFixture returns a deterministic C-strkey and the base64 LedgerKey of
// its persistent ScvLedgerKeyContractInstance entry — the key the explorer's
// contract routes classify.
func instanceFixture(t *testing.T, tag string) (contractStrkey, instanceKeyXDR string) {
	t.Helper()
	sum := sha256.Sum256([]byte("contract-instance-state-it-" + tag))
	contractStrkey, err := strkey.Encode(strkey.VersionByteContract, sum[:])
	if err != nil {
		t.Fatalf("encode contract strkey: %v", err)
	}
	cid := xdr.ContractId(sum)
	key := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	instanceKeyXDR, err = xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal instance key: %v", err)
	}
	return contractStrkey, instanceKeyXDR
}

// TestContractInstanceState_TTLRowAndAbsence runs the explorer's instance read
// against a real ClickHouse: a TTL row for the instance key yields Known plus
// the newest live_until (so an archived instance is judged archived), and a
// contract with no lake evidence at all yields Known=false without error.
func TestContractInstanceState_TTLRowAndAbsence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	archivedContract, archivedKey := instanceFixture(t, "archived")
	neverDeployed, _ := instanceFixture(t, "never-deployed")
	lapsedAt := ttlAsOf - 1_000_000

	rows := []chstore.LedgerEntryChangeRow{
		ttlChangeRow(archivedKey, 72_100_001, 1, lapsedAt-5, 48),
		ttlChangeRow(archivedKey, 72_100_002, 1, lapsedAt, 48),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	defer func() { _ = er.Close() }()

	st, err := er.ContractInstanceState(ctx, archivedContract)
	if err != nil {
		t.Fatalf("ContractInstanceState(archived): %v", err)
	}
	if !st.Known || st.LiveUntil != lapsedAt {
		t.Fatalf("archived instance state = %+v, want Known with LiveUntil %d", st, lapsedAt)
	}
	if v := chstore.TTLVerdictAt(st.LiveUntil, ttlAsOf); v != chstore.TTLArchived {
		t.Errorf("verdict = %v, want TTLArchived", v)
	}

	st, err = er.ContractInstanceState(ctx, neverDeployed)
	if err != nil {
		t.Fatalf("ContractInstanceState(never deployed): %v", err)
	}
	if st.Known || st.LiveUntil != 0 {
		t.Errorf("never-deployed state = %+v, want zero", st)
	}
}

// TestClickHouseStringTopicPrefilterAdmitsPhoenixPool is the live-ClickHouse
// proof for the lake half of the string-topic prefilter.
//
// StreamContractEventsFiltered's topic[0] prefilter must not be
// `topic_0_sym IN (…)` alone: extract.go fills that column from
// `Topics[0].GetSym()` — Symbol ONLY — so it is EMPTY for every event whose
// topic[0] is an ScvString. Phoenix's factory publishes
// ("create","liquidity_pool") as two Strings, so every consumer that asks the
// lake for creationSym "create" (seed-protocol-contracts, and the -ch
// re-derive's gatedPrefilter walk) matched ZERO rows over a lake that holds
// those events from ledger 51,572,026 — the walk looked clean and admitted
// nothing.
//
// The row inserted below is the REAL r1 capture, byte-for-byte: contract id,
// both topic blobs, the body and the empty topic_0_sym are copied from
// test/fixtures/phoenix/factory-create/
// factory_2026-07-02_ledgers_63293663-63293708.jsonl (ledger 63,293,708, the
// factory's create of CBENABXP…). Only the ledger/tx coordinates are moved
// into this test's private range.
//
// The assertion runs the whole loop the defect broke — lake row → SQL
// prefilter → streamed event → decoder → identity gate — and ends on the one
// observation that cannot be faked: the registry's live-upsert hook receiving
// the announced pool. Reverting topic0Predicate to `topic_0_sym IN (…)` makes
// the stream return 1 row instead of 2 and seeds nothing.
func TestClickHouseStringTopicPrefilterAdmitsPhoenixPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		lo = uint32(70_300_001)
		hi = uint32(70_300_003)
		// The real phoenix factory, and the pool the captured event
		// announces (body decoded: a single ScvAddress).
		factoryContract = phoenix.MainnetFactory
		announcedPool   = "CBENABXP6C4C7WG6KB7JQOTDS5GIIXF3IX3PIYNZFCDZDWUHITO2HZ4S"
		// Verbatim from the capture: ScvString("create"),
		// ScvString("liquidity_pool") and the ScvAddress body.
		realCreateTopic0 = "AAAADgAAAAZjcmVhdGUAAA=="
		realCreateTopic1 = "AAAADgAAAA5saXF1aWRpdHlfcG9vbAAA"
		realCreateBody   = "AAAAEgAAAAFI0Abv8Lgv2N5Qfpg6Y5dMhFy7Rfb0Ybkoh5Hah0Tdow=="
		// The factory's other captured event, ("Factory","Updated Config"):
		// also String topics, also empty topic_0_sym. It must NOT match.
		decoyTopic0 = "AAAADgAAAAdGYWN0b3J5AA=="
		decoyTopic1 = "AAAADgAAAA5VcGRhdGVkIENvbmZpZwAA"
		decoyBody   = "AAAAAQ=="

		txCreate = "1111111111111111111111111111111111111111111111111111111111111111"
		txSymbol = "2222222222222222222222222222222222222222222222222222222222222222"
		txDecoy  = "3333333333333333333333333333333333333333333333333333333333333333"
	)
	closeTime := time.Date(2026, 7, 2, 10, 24, 5, 0, time.UTC)

	row := func(ledger uint32, tx string, topics []string, topic0Sym, body string) chstore.ContractEventRow {
		return chstore.ContractEventRow{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: tx, OpIndex: 0, EventIndex: 0,
			ContractID: factoryContract, EventType: "contract",
			TopicCount: uint8(len(topics)), //nolint:gosec // two topics, fixed above.
			Topic0Sym:  topic0Sym, TopicsXDR: topics, DataXDR: body,
			OpArgsXDR: []string{}, InSuccessfulCall: 1,
		}
	}

	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: lo, CloseTime: closeTime, LedgerHash: "aa11aa11", PrevHash: "bb22bb22",
			ProtocolVersion: 23, TxCount: 3, OpCount: 3, SorobanEventCount: 3,
		},
		Events: []chstore.ContractEventRow{
			// (1) The real String-topic create: topic_0_sym EMPTY, exactly as
			// extract.go writes it.
			row(lo, txCreate, []string{realCreateTopic0, realCreateTopic1}, "", realCreateBody),
			// (2) A Symbol-topic "create" from the same emitter — the arm that
			// already worked. Pinned so widening the predicate cannot silently
			// drop the encoding every other gated source relies on.
			row(lo+1, txSymbol, []string{scval.MustEncodeSymbol("create"), realCreateTopic1},
				"create", realCreateBody),
			// (3) A different String topic[0] from the same emitter: must be
			// excluded, or the predicate has stopped filtering.
			row(lo+2, txDecoy, []string{decoyTopic0, decoyTopic1}, "", decoyBody),
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

	// The prefilter as seed-protocol-contracts and gatedPrefilter issue it:
	// scope to the factory, ask for creationSym "create".
	var streamed []events.Event
	if err := chstore.StreamContractEventsFiltered(ctx, addr, lo, hi,
		[]string{factoryContract}, []string{phoenix.EventActionCreate}, nil,
		true, false, false,
		func(ev events.Event) error {
			streamed = append(streamed, ev)
			return nil
		}); err != nil {
		t.Fatalf("StreamContractEventsFiltered: %v", err)
	}

	if len(streamed) != 2 {
		t.Fatalf("prefilter on creationSym %q returned %d events, want 2 (the ScvString create AND "+
			"the ScvSymbol create; the ScvString one has an EMPTY topic_0_sym, so a "+
			"topic_0_sym-only predicate returns just 1 and the phoenix factory walk admits nothing)",
			phoenix.EventActionCreate, len(streamed))
	}
	byTx := map[string]events.Event{}
	for _, ev := range streamed {
		byTx[ev.TxHash] = ev
	}
	if _, ok := byTx[txCreate]; !ok {
		t.Errorf("the real ScvString ((\"create\",\"liquidity_pool\")) event did not survive the "+
			"prefilter; streamed tx hashes = %v", streamedTxHashes(byTx))
	}
	if _, ok := byTx[txSymbol]; !ok {
		t.Errorf("the ScvSymbol(\"create\") event did not survive the prefilter — widening the "+
			"predicate must not drop the topic_0_sym arm; streamed tx hashes = %v", streamedTxHashes(byTx))
	}
	if _, ok := byTx[txDecoy]; ok {
		t.Error("the (\"Factory\",\"Updated Config\") event survived a prefilter for \"create\" — " +
			"the predicate has stopped filtering")
	}

	// Close the loop: the streamed lake row must actually admit the pool.
	// Seeding is the only observable a decoder that merely RECOGNISES the
	// event cannot produce.
	var seeded []string
	dec := phoenix.NewDecoder(contractid.WithHook(func(child, factory string, ledger uint32) {
		if factory != factoryContract {
			t.Errorf("seeded %s with provenance factory %s, want %s", child, factory, factoryContract)
		}
		seeded = append(seeded, child)
	}))
	createEvent, ok := byTx[txCreate]
	if !ok {
		t.Fatal("cannot run the admission leg: the create event was filtered out above")
	}
	if !dec.Matches(createEvent) {
		t.Fatalf("phoenix decoder rejects the factory's create event as streamed from the lake")
	}
	if _, err := dec.Decode(createEvent); err != nil {
		t.Fatalf("decode streamed create event: %v", err)
	}
	if len(seeded) != 1 || seeded[0] != announcedPool {
		t.Fatalf("lake-streamed create event seeded %v, want exactly [%s] — the announced pool must "+
			"reach the identity gate for a factory-created pool's swaps to be attributed (F048)",
			seeded, announcedPool)
	}
}

// streamedTxHashes returns the streamed tx hashes, for a readable
// failure message.
func streamedTxHashes(m map[string]events.Event) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// opKey orders like the stellar.operations sort key.
type opKey struct{ L, T, O uint32 }

func (a opKey) less(b opKey) bool {
	if a.L != b.L {
		return a.L < b.L
	}
	if a.T != b.T {
		return a.T < b.T
	}
	return a.O < b.O
}

// TestOperationsTypeFilter_PagesSparseTypeWithoutGapOrDup walks
// /v1/operations?type= through the real handler and reader over a sparse
// type: matches sit on and beside the 5,000-ledger scan floors, several share
// a ledger and a transaction, one is an un-merged duplicate part, and payments
// are interleaved as non-matches. Every limit must return each match exactly
// once, newest first, with every cursor strictly below the last.
func TestOperationsTypeFilter_PagesSparseTypeWithoutGapOrDup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, "SYSTEM STOP MERGES stellar.operations"); err != nil {
		t.Fatalf("SYSTEM STOP MERGES: %v", err)
	}
	t.Cleanup(func() { _ = raw.Exec(context.Background(), "SYSTEM START MERGES stellar.operations") })

	// A range no other test seeds; the walk starts above it and stops below it.
	const top = uint32(3_500_000_000)
	const window = 5000
	start := top + 1
	floor := func(k uint32) uint32 { return start - k*window } // short-page scan floors
	bottom := floor(4) - 1

	matches := []opKey{
		{top, 2, 0},
		{top, 2, 1},
		{top, 2, 2},
		{top, 5, 0},
		{floor(1), 0, 0},
		{floor(1) - 1, 0, 0},
		{floor(1) - 1, 1, 0},
		{floor(2), 3, 1},
		{floor(2) - 1, 0, 0},
		{floor(3), 0, 0},
		{floor(4) + 9, 0, 0},
	}
	nonMatches := []opKey{{top, 2, 3}, {floor(1), 0, 1}, {floor(2), 3, 0}, {floor(3) + 1, 0, 0}, {bottom, 0, 0}}
	insert := func(k opKey, opType, ingestedAt string) { insertLakeOp(ctx, t, raw, k, opType, ingestedAt) }
	for _, k := range matches {
		insert(k, "OperationTypeInflation", "2026-09-01 00:00:00")
	}
	for _, k := range nonMatches {
		insert(k, "OperationTypePayment", "2026-09-01 00:00:00")
	}
	dup := matches[4]
	insert(dup, "OperationTypeInflation", "2026-09-02 00:00:00") // a second, un-merged part
	// Physical rows on purpose: the walk must collapse exactly these two.
	var copies []struct {
		IngestedAt time.Time `ch:"ingested_at"`
	}
	if err := raw.Select(ctx, &copies, `SELECT ingested_at FROM stellar.operations WHERE ledger_seq = ? AND tx_index = 0 AND op_index = 0`, dup.L); err != nil || len(copies) != 2 {
		t.Fatalf("duplicate rows = %d (err %v), want 2 un-merged copies", len(copies), err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	for _, limit := range []int{1, 2, 3, 50} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			assertServedExactly(t, walkTypedOperations(t, er, limit, opKey{start, 0, 0}, bottom), matches)
		})
	}
}

func insertLakeOp(ctx context.Context, t *testing.T, raw driver.Conn, k opKey, opType, ingestedAt string) {
	t.Helper()
	q := fmt.Sprintf(`INSERT INTO stellar.operations
		(ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr, ingested_at)
		VALUES (%d, toDateTime('2030-01-01 00:00:00', 'UTC'), '%064x', %d, %d, '%s', '', '', toDateTime('%s', 'UTC'))`,
		k.L, uint64(k.L)<<8|uint64(k.T), k.T, k.O, opType, ingestedAt)
	if err := raw.Exec(ctx, q); err != nil {
		t.Fatalf("insert %v: %v", k, err)
	}
}

// assertServedExactly: walkTypedOperations already proved strict descent, so
// equal length plus full membership means each match exactly once.
func assertServedExactly(t *testing.T, got, matches []opKey) {
	t.Helper()
	if len(got) != len(matches) {
		t.Fatalf("walk returned %d ops, want %d: %v", len(got), len(matches), got)
	}
	seen := map[opKey]bool{}
	for _, k := range got {
		seen[k] = true
	}
	for _, k := range matches {
		if !seen[k] {
			t.Errorf("match %v never served (gap)", k)
		}
	}
}

func typedOpsHandler(t *testing.T, er *chstore.ExplorerReader, page *explorer.OperationsView) *explorer.Handler {
	capture := func(w http.ResponseWriter, data any) {
		*page, _ = data.(explorer.OperationsView)
		w.WriteHeader(http.StatusOK)
	}
	return &explorer.Handler{
		Reader: er,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ParseLimit: func(_ http.ResponseWriter, r *http.Request, def, _ int) (int, bool) {
			n, err := strconv.Atoi(r.URL.Query().Get("limit"))
			if err != nil {
				return def, true
			}
			return n, true
		},
		LakeWatermark: func(context.Context) (uint32, bool, bool) { return 0, false, false },
		ClientAborted: func(*http.Request, error) bool { return false },
		WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, title string, status int, detail string) {
			t.Errorf("problem %d %s: %s", status, title, detail)
			w.WriteHeader(status)
		},
		WriteJSON:   func(w http.ResponseWriter, data any, _ bool) { capture(w, data) },
		WriteJSONAt: func(w http.ResponseWriter, data any, _, _ bool, _ time.Time) { capture(w, data) },
	}
}

// appendTypedPage appends a page's ops at or above bottom, failing unless
// each is strictly below the request cursor and the previous op.
func appendTypedPage(t *testing.T, out []opKey, ops []explorer.OpView, cur opKey, bottom uint32) []opKey {
	t.Helper()
	for _, o := range ops {
		k := opKey{o.Ledger, o.TxIndex, o.OpIndex}
		if !k.less(cur) {
			t.Fatalf("cursor %v served %v, not strictly below it", cur, k)
		}
		if n := len(out); n > 0 && !k.less(out[n-1]) {
			t.Fatalf("op %v after %v: duplicate or out of order", k, out[n-1])
		}
		if k.L >= bottom {
			out = append(out, k)
		}
	}
	return out
}

// walkTypedOperations pages ?type=inflation from cursor `from` until the
// cursor drops below `bottom`, failing on any ordering or cursor regression.
func walkTypedOperations(t *testing.T, er *chstore.ExplorerReader, limit int, from opKey, bottom uint32) []opKey {
	t.Helper()
	var page explorer.OperationsView
	h := typedOpsHandler(t, er, &page)
	var out []opKey
	cur := from
	for pages := 0; cur.L >= bottom; pages++ {
		if pages > 64 {
			t.Fatalf("no progress after %d pages at cursor %v", pages, cur)
		}
		page = explorer.OperationsView{}
		rec := httptest.NewRecorder()
		h.Operations(rec, httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/v1/operations?type=inflation&limit=%d&cursor=%d.%d.%d", limit, cur.L, cur.T, cur.O), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("cursor %v: status %d", cur, rec.Code)
		}
		if len(page.Operations) > limit {
			t.Fatalf("cursor %v: %d ops, limit %d", cur, len(page.Operations), limit)
		}
		out = appendTypedPage(t, out, page.Operations, cur, bottom)
		if page.NextCursor == "" {
			break
		}
		next := parseOpCursor(t, page.NextCursor)
		if !next.less(cur) {
			t.Fatalf("next_cursor %v does not move below %v", next, cur)
		}
		cur = next
	}
	return out
}

func parseOpCursor(t *testing.T, s string) opKey {
	t.Helper()
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		t.Fatalf("cursor %q is not ledger.tx.op", s)
	}
	var n [3]uint32
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			t.Fatalf("cursor %q: %v", s, err)
		}
		n[i] = uint32(v)
	}
	return opKey{n[0], n[1], n[2]}
}

// TestExplorerScanQueries_ExecuteAgainstServer proves, against a REAL
// ClickHouse server, that every scan-shaped explorer query (extracted to
// builders + pinned with
// `SETTINGS max_threads/max_memory_usage`) still parses and executes — the
// unit tests pin the SQL text; this pins that the text is valid ClickHouse
// (a misplaced SETTINGS clause or a drifted placeholder count fails HERE,
// not in production). Result contents are asserted only where a seeded row
// exercises a code path that would otherwise short-circuit before its
// query (AccountState's trustline/offer reads run only for an EXISTING
// account).
func TestExplorerScanQueries_ExecuteAgainstServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// A real, checksum-valid account with a real AccountEntry in the
	// current-state projection, so AccountState proceeds past the entry
	// lookup into the pinned trustline + offer scans.
	var seed [32]byte
	seed[0] = 0xE5
	account, err := strkey.Encode(strkey.VersionByteAccountID, seed[:])
	if err != nil {
		t.Fatalf("encode account strkey: %v", err)
	}
	var aid xdr.AccountId
	if err := aid.SetAddress(account); err != nil {
		t.Fatalf("set address: %v", err)
	}
	keyB64, err := xdr.MarshalBase64(xdr.LedgerKey{
		Type:    xdr.LedgerEntryTypeAccount,
		Account: &xdr.LedgerKeyAccount{AccountId: aid},
	})
	if err != nil {
		t.Fatalf("marshal account key: %v", err)
	}
	entryB64, err := xdr.MarshalBase64(xdr.LedgerEntry{
		LastModifiedLedgerSeq: 71_000_001,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeAccount,
			Account: &xdr.AccountEntry{
				AccountId:  aid,
				Balance:    5_000_000,
				SeqNum:     7,
				Thresholds: xdr.Thresholds{1, 0, 0, 0},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal account entry: %v", err)
	}
	// The trustline + offer rows carry GENUINE LedgerKey XDR: the reader's
	// trustline/offer scans are PK-prefix range reads (`key_xdr LIKE
	// '<52-char real-XDR prefix>%'`, accountEntryKeyPrefix), so a synthetic
	// placeholder key can never match and would silently skip the very path
	// under test.
	var issuerSeed [32]byte
	issuerSeed[0] = 0xE6
	issuer, err := strkey.Encode(strkey.VersionByteAccountID, issuerSeed[:])
	if err != nil {
		t.Fatalf("encode issuer strkey: %v", err)
	}
	var issuerAID xdr.AccountId
	if err := issuerAID.SetAddress(issuer); err != nil {
		t.Fatalf("set issuer address: %v", err)
	}
	var code4 [4]byte
	copy(code4[:], "USDX")
	tlAsset := xdr.TrustLineAsset{
		Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{AssetCode: code4, Issuer: issuerAID},
	}
	assetID := "USDX-" + issuer
	tlKeyB64, err := xdr.MarshalBase64(xdr.LedgerKey{
		Type:      xdr.LedgerEntryTypeTrustline,
		TrustLine: &xdr.LedgerKeyTrustLine{AccountId: aid, Asset: tlAsset},
	})
	if err != nil {
		t.Fatalf("marshal trustline key: %v", err)
	}
	tlEntryB64, err := xdr.MarshalBase64(xdr.LedgerEntry{
		LastModifiedLedgerSeq: 71_000_001,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeTrustline,
			TrustLine: &xdr.TrustLineEntry{
				AccountId: aid, Asset: tlAsset,
				Balance: 42, Limit: 1_000_000,
				Flags: xdr.Uint32(xdr.TrustLineFlagsAuthorizedFlag),
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal trustline entry: %v", err)
	}
	const offerID = xdr.Int64(9_001)
	offerKeyB64, err := xdr.MarshalBase64(xdr.LedgerKey{
		Type:  xdr.LedgerEntryTypeOffer,
		Offer: &xdr.LedgerKeyOffer{SellerId: aid, OfferId: offerID},
	})
	if err != nil {
		t.Fatalf("marshal offer key: %v", err)
	}
	offerEntryB64, err := xdr.MarshalBase64(xdr.LedgerEntry{
		LastModifiedLedgerSeq: 71_000_001,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeOffer,
			Offer: &xdr.OfferEntry{
				SellerId: aid, OfferId: offerID,
				Selling: xdr.Asset{Type: xdr.AssetTypeAssetTypeNative},
				Buying: xdr.Asset{
					Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
					AlphaNum4: &xdr.AlphaNum4{AssetCode: code4, Issuer: issuerAID},
				},
				Amount: 1_500, Price: xdr.Price{N: 3, D: 2},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal offer entry: %v", err)
	}
	closeTime := time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC)
	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: 71_000_001, CloseTime: closeTime, TxHash: "e5a1", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "created", EntryType: "account",
			KeyXDR: keyB64, EntryXDR: entryB64, AccountID: account, Balance: 5_000_000,
		},
		{
			LedgerSeq: 71_000_001, CloseTime: closeTime, TxHash: "e5a1", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 2, ChangeType: "created", EntryType: "trustline",
			KeyXDR: tlKeyB64, EntryXDR: tlEntryB64, AccountID: account,
			Asset: assetID, Balance: 42,
		},
		{
			LedgerSeq: 71_000_001, CloseTime: closeTime, TxHash: "e5a1", OpIndex: 2, ChangeIndex: 0,
			IntraLedgerSeq: 3, ChangeType: "created", EntryType: "offer",
			KeyXDR: offerKeyB64, EntryXDR: offerEntryB64, AccountID: account,
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	// The account-history readers refuse over an EMPTY ops_by_source, which
	// would skip their builders; one sourced row keeps them under test.
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, `INSERT INTO stellar.ops_by_source
		(source_account, ledger_seq, tx_index, op_index) VALUES (?, 71000001, 0, 0)`, account); err != nil {
		t.Fatalf("seed ops_by_source: %v", err)
	}

	r, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	contract, err := strkey.Encode(strkey.VersionByteContract, seed[:])
	if err != nil {
		t.Fatalf("encode contract strkey: %v", err)
	}
	cursor := chstore.ExplorerCursor{Ledger: 71_000_002, A: 1, B: 1}

	// Every pinned builder executes without a server-side parse/settings
	// error. Empty results are fine — validity, not content, is under test.
	for name, call := range map[string]func() error{
		"RecentOperations(first page)": func() error {
			_, err := r.RecentOperations(ctx, 5, chstore.ExplorerCursor{})
			return err
		},
		"RecentOperations(cursor)": func() error {
			_, err := r.RecentOperations(ctx, 5, cursor)
			return err
		},
		"RecentOperationsOfType(first page)": func() error {
			_, err := r.RecentOperationsOfType(ctx, 5, chstore.ExplorerCursor{}, []string{"OperationTypePayment", "OperationTypeClawback"})
			return err
		},
		"RecentOperationsOfType(cursor)": func() error {
			_, err := r.RecentOperationsOfType(ctx, 5, cursor, []string{"OperationTypePayment"})
			return err
		},
		"OperationTypeStats": func() error {
			_, err := r.OperationTypeStats(ctx, 0)
			return err
		},
		"AccountTransactions(first page)": func() error {
			_, _, err := r.AccountTransactions(ctx, account, 5, chstore.ExplorerCursor{})
			return err
		},
		"AccountTransactions(cursor)": func() error {
			_, _, err := r.AccountTransactions(ctx, account, 5, cursor)
			return err
		},
		"AccountOperations(first page)": func() error {
			_, _, err := r.AccountOperations(ctx, account, 5, chstore.ExplorerCursor{})
			return err
		},
		"AccountOperations(cursor)": func() error {
			_, _, err := r.AccountOperations(ctx, account, 5, cursor)
			return err
		},
		"ContractEventsRecent(first page)": func() error {
			_, err := r.ContractEventsRecent(ctx, contract, 5, chstore.ContractEventsCursor{})
			return err
		},
		"ContractEventsRecent(cursor)": func() error {
			_, err := r.ContractEventsRecent(ctx, contract, 5, chstore.ContractEventsCursor{
				Ledger: cursor.Ledger, TxHash: "ff", OpIndex: cursor.A, EventIndex: cursor.B,
			})
			return err
		},
		"RecentContracts": func() error {
			_, err := r.RecentContracts(ctx, 5, 0)
			return err
		},
		"ContractInteractions": func() error {
			_, _, err := r.ContractInteractions(ctx, contract, 5, 0)
			return err
		},
		"ContractCodeHistory": func() error {
			_, err := r.ContractCodeHistory(ctx, contract)
			return err
		},
		"AssetHolders": func() error {
			_, _, err := r.AssetHolders(ctx, assetID, 5)
			return err
		},
		"AccountsByWealth": func() error {
			_, err := r.AccountsByWealth(ctx, []string{"native"}, []string{"0.4"}, 5)
			return err
		},
		"AccountsUnspendable": func() error {
			_, err := r.AccountsUnspendable(ctx, []string{account})
			return err
		},
		"AccountMovements(filter+cursor)": func() error {
			_, err := r.AccountMovements(ctx, account, 5,
				chstore.AccountMovementCursor{Ledger: 71_000_002, TxHash: "ff"},
				chstore.AccountMovementFilter{Kind: "payment", Direction: chstore.AccountMovementSent, Asset: "native"})
			return err
		},
	} {
		if err := call(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// The seeded account exercises the pinned trustline/offer scans through
	// AccountState and must resolve with its real balance + trustline.
	st, err := r.AccountState(ctx, account)
	if err != nil {
		t.Fatalf("AccountState: %v", err)
	}
	if !st.Exists || st.Balance != 5_000_000 {
		t.Errorf("AccountState exists=%v balance=%d, want the seeded entry (true, 5000000)", st.Exists, st.Balance)
	}
	if len(st.Trustlines) != 1 || st.Trustlines[0].Balance != 42 || st.Trustlines[0].Limit != 1_000_000 {
		t.Errorf("AccountState trustlines = %+v, want the seeded 42-balance / 1000000-limit trustline", st.Trustlines)
	}
	// The offer arm is the same PK-prefix range shape; the seeded offer's
	// real LedgerKey must round-trip through it.
	if len(st.Offers) != 1 || st.Offers[0].OfferID != 9_001 || st.Offers[0].Amount != 1_500 {
		t.Errorf("AccountState offers = %+v, want the seeded offer 9001 (amount 1500)", st.Offers)
	}

	// The seeded trustline also proves the holders board end to end.
	holders, total, err := r.AssetHolders(ctx, assetID, 5)
	if err != nil || total != 1 || len(holders) != 1 || holders[0].Balance != 42 {
		t.Errorf("AssetHolders = %v total=%d err=%v, want the one seeded holder", holders, total, err)
	}
}

// TestContractCodeHistory_ServesTheSeededTimeline asserts ContractCodeHistory's
// returned VALUES on a real server, down both of its paths: first served by
// the keyed contract_instance_changes index (populated by the shipped MV),
// then — with this contract's index rows deleted while the table stays
// non-empty — as an unproven per-contract miss: the index's
// emptiness for this contract is not proof it never upgraded, so the reader
// falls back to the changes-log scan, which still holds the instance writes
// and returns the same real timeline. The seeded timeline carries an
// instance-storage rewrite that keeps the executable (must collapse onto the
// FIRST ledger that installed it) and is inserted out of order (the read
// must sort by ledger).
func TestContractCodeHistory_ServesTheSeededTimeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	var cidRaw, keeperRaw xdr.Hash
	copy(cidRaw[:], "t427-code-history-timeline-cid")
	copy(keeperRaw[:], "t427-code-history-keeper-cid")
	cid, keeper := xdr.ContractId(cidRaw), xdr.ContractId(keeperRaw)
	contract, err := strkey.Encode(strkey.VersionByteContract, cidRaw[:])
	if err != nil {
		t.Fatalf("encode contract strkey: %v", err)
	}
	h0, h1, hKeeper := t356Hash(0x70), t356Hash(0x71), t356Hash(0x72)
	instKey, entry0 := t356InstanceKeyAndEntry(t, cid, h0)
	_, entry1 := t356InstanceKeyAndEntry(t, cid, h1)
	keeperKey, keeperEntry := t356InstanceKeyAndEntry(t, keeper, hKeeper)

	const deploy, rewrite, upgrade = uint32(72_427_010), uint32(72_427_020), uint32(72_427_030)
	t1 := time.Date(2025, 4, 1, 0, 0, 10, 0, time.UTC)
	t2, t3 := t1.Add(10*time.Second), t1.Add(20*time.Second)
	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: upgrade, CloseTime: t3, TxHash: "t427-upgrade", IntraLedgerSeq: 5,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry1,
		},
		{
			LedgerSeq: deploy, CloseTime: t1, TxHash: "t427-deploy", IntraLedgerSeq: 2,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry0,
		},
		{
			LedgerSeq: rewrite, CloseTime: t2, TxHash: "t427-storage", IntraLedgerSeq: 7,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry0,
		},
		{
			LedgerSeq: deploy, CloseTime: t1, TxHash: "t427-keeper", IntraLedgerSeq: 3,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: keeperKey, EntryXDR: keeperEntry,
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	want := []chstore.ContractCodeVersion{
		{Ledger: deploy, CloseTime: t1, WasmHash: hex.EncodeToString(h0[:])},
		{Ledger: upgrade, CloseTime: t3, WasmHash: hex.EncodeToString(h1[:])},
	}

	conn := dialClickHouse(t, ctx, "stellar")
	cidHex, keeperHex := hex.EncodeToString(cidRaw[:]), hex.EncodeToString(keeperRaw[:])
	if n := countInstanceIndexRows(t, ctx, conn, cidHex); n != 3 {
		t.Fatalf("contract_instance_changes holds %d rows for the contract, want the 3 seeded writes", n)
	}
	assertCodeHistory(t, ctx, addr, contract, "index-served", want)

	// Index miss for this contract on a non-empty (so "available") index is
	// NOT authoritative: instanceChangesIndexAvailable only proves the table
	// isn't globally empty, not that this contract's backfill has landed, so
	// the reader must fall back to the ~30 s key_xdr scan of the changes log
	// and return its real (non-empty) answer.
	syncCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"mutations_sync": "2"}))
	if err := conn.Exec(syncCtx,
		`ALTER TABLE stellar.contract_instance_changes DELETE WHERE contract_hash = ?`, cidHex); err != nil {
		t.Fatalf("delete index rows: %v", err)
	}
	if n := countInstanceIndexRows(t, ctx, conn, cidHex); n != 0 {
		t.Fatalf("contract_instance_changes still holds %d rows for the contract after the delete", n)
	}
	if n := countInstanceIndexRows(t, ctx, conn, keeperHex); n != 1 {
		t.Fatalf("keeper contract has %d index rows, want 1 (the index must stay non-empty)", n)
	}
	assertCodeHistory(t, ctx, addr, contract, "index miss falls back to legacy scan", want)
}

// TestContractCodeHistory_KeepsOldExecutableBeyondRawWriteCap seeds more raw
// instance writes than contractCodeHistoryMaxRows, all after the contract's
// first executable (and a return to it): a raw-row cap would drop the oldest
// executable; collapsing before the cap must keep the whole A->B->A timeline.
func TestContractCodeHistory_KeepsOldExecutableBeyondRawWriteCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	var cidRaw xdr.Hash
	copy(cidRaw[:], "inv1752-code-history-cap-cid")
	cid := xdr.ContractId(cidRaw)
	contract, err := strkey.Encode(strkey.VersionByteContract, cidRaw[:])
	if err != nil {
		t.Fatalf("encode contract strkey: %v", err)
	}
	hA, hB := t356Hash(0x80), t356Hash(0x81)
	instKey, entryA := t356InstanceKeyAndEntry(t, cid, hA)
	_, entryB := t356InstanceKeyAndEntry(t, cid, hB)

	const base, rewrites = uint32(73_752_000), 10_050
	t0 := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	row := func(seq uint32, entry string) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			LedgerSeq: seq, CloseTime: t0.Add(time.Duration(seq-base) * time.Second),
			TxHash: fmt.Sprintf("inv1752-%d", seq), IntraLedgerSeq: 1,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry,
		}
	}
	rows := []chstore.LedgerEntryChangeRow{row(base, entryA)}
	for i := uint32(1); i <= rewrites; i++ {
		rows = append(rows, row(base+i, entryB))
	}
	rows = append(rows, row(base+rewrites+1, entryA))
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	want := []chstore.ContractCodeVersion{
		{Ledger: base, CloseTime: t0, WasmHash: hex.EncodeToString(hA[:])},
		{Ledger: base + 1, CloseTime: t0.Add(time.Second), WasmHash: hex.EncodeToString(hB[:])},
		{Ledger: base + rewrites + 1, CloseTime: t0.Add((rewrites + 1) * time.Second), WasmHash: hex.EncodeToString(hA[:])},
	}
	assertCodeHistory(t, ctx, addr, contract, "index-served beyond the raw-write cap", want)
}

func countInstanceIndexRows(t *testing.T, ctx context.Context, conn driver.Conn, contractHash string) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.contract_instance_changes FINAL
		WHERE contract_hash = ?`, contractHash).Scan(&n); err != nil {
		t.Fatalf("count contract_instance_changes: %v", err)
	}
	return n
}

// assertCodeHistory reads through a FRESH reader so its index-availability
// probe reflects the table as it is now, not a cached earlier verdict.
func assertCodeHistory(t *testing.T, ctx context.Context, addr, contract, leg string, want []chstore.ContractCodeVersion) {
	t.Helper()
	r, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("%s: NewExplorerReader: %v", leg, err)
	}
	defer func() { _ = r.Close() }()
	got, err := r.ContractCodeHistory(ctx, contract)
	if err != nil {
		t.Fatalf("%s: ContractCodeHistory: %v", leg, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: code history = %+v, want %+v", leg, got, want)
	}
	for i := range want {
		if got[i].Ledger != want[i].Ledger || got[i].WasmHash != want[i].WasmHash ||
			!got[i].CloseTime.Equal(want[i].CloseTime) {
			t.Fatalf("%s: code history[%d] = %+v, want %+v (full %+v)", leg, i, got[i], want[i], got)
		}
	}
}

// This file is the live-ClickHouse cost proof for the op-stream lake readers
// (StreamSDEXOps, StreamClassicOps). It pins TWO properties that a text grep
// for "CreatingSet" or "Join" cannot tell apart, because three different query
// shapes satisfy one each:
//
//	IN-subquery   (the defect): operation_results IS pruned, but the
//	              window's whole successful-tx set is materialised first —
//	              CreatingSetsTransform, a 10 GiB memory blowout.
//	derived-outer (the rejected first fix): no set-build, but the outer ledger
//	              window is hoisted into a derived table, so ClickHouse cannot
//	              propagate it through o.ledger_seq = r.ledger_seq and
//	              stellar.operation_results full-scans as grace_hash's spilled
//	              build side. One unbounded read traded for another.
//	shipped       (contractCallOpsQuery's shape): the successful-tx join spelled
//	              BEFORE the outer WHERE — no set-build AND pruned.
//
// Both rejected shapes are frozen below as oracles and both are asserted to
// still exhibit their pathology against this fixture, so neither assertion can
// pass vacuously.

const (
	// A ledger range of this test's own (partitions 30..31), clear of every
	// other ClickHouse integration fixture's.
	opsPruneBase = uint32(30_500_001)
	// One op / result / tx per ledger. ~123 granules of operation_results, so
	// the 1,000-ledger read window is one granule when pruning works and all
	// of them when it does not.
	opsPruneRows = 1_000_000
	// The window every query under test reads.
	opsPruneWindow = uint32(1_000)
	// Every opsPruneFailMod'th tx is a FAILED tx: the successful-tx filter has
	// to actually exclude rows, or the differential proves nothing.
	opsPruneFailMod = 7
	// A re-ingested duplicate PART of stellar.transactions covering the read
	// window. These readers take no FINAL, so it puts the same tx_hash on the
	// join's build side twice — what `GROUP BY tx_hash` in the derived table
	// exists to absorb.
	opsPruneDupRows = 5_000

	opsPruneSDEXType    = "OperationTypeManageSellOffer"
	opsPruneClassicType = "OperationTypePayment"
)

// ── the shapes this test exists to rule out ─────────────────────────────────

// inSubqueryOpsSQL is the IN-subquery successful-tx filter that sdexOpsQuery
// and classicOpsQuery must not use: its CreatingSet step materialises the
// whole window's tx-hash set before the join runs. The sibling
// contractCallOpsQuery is likewise off this shape.
func inSubqueryOpsSQL(opTypes string, from, to uint32) string {
	return fmt.Sprintf(`
		SELECT o.ledger_seq, o.close_time, o.tx_hash, o.op_index, o.source_account,
		       o.body_xdr, r.result_xdr
		FROM stellar.operations AS o
		INNER JOIN stellar.operation_results AS r
		  ON o.ledger_seq = r.ledger_seq AND o.tx_hash = r.tx_hash AND o.op_index = r.op_index
		WHERE o.ledger_seq BETWEEN %d AND %d
		  AND o.op_type IN (%s)
		  AND o.tx_hash IN (
		      SELECT tx_hash FROM stellar.transactions
		      WHERE successful = 1 AND ledger_seq BETWEEN %d AND %d
		  )
		ORDER BY o.ledger_seq, o.tx_hash, o.op_index
		SETTINGS join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 32`,
		from, to, opTypes, from, to)
}

// derivedWindowOpsSQL is the REJECTED first remediation: the IN-set is gone,
// but the outer ledger window is wrapped in a derived table, which costs
// stellar.operation_results its primary-key pruning entirely.
func derivedWindowOpsSQL(opTypes string, from, to uint32) string {
	return fmt.Sprintf(`
		SELECT o.ledger_seq, o.close_time, o.tx_hash, o.op_index, o.source_account,
		       o.body_xdr, r.result_xdr
		FROM (
		    SELECT ledger_seq, close_time, tx_hash, op_index, source_account, body_xdr
		    FROM stellar.operations
		    WHERE ledger_seq BETWEEN %d AND %d
		      AND op_type IN (%s)
		) AS o
		INNER JOIN (
		    SELECT tx_hash FROM stellar.transactions
		    WHERE successful = 1 AND ledger_seq BETWEEN %d AND %d
		    GROUP BY tx_hash
		) AS t ON o.tx_hash = t.tx_hash
		INNER JOIN stellar.operation_results AS r
		  ON o.ledger_seq = r.ledger_seq AND o.tx_hash = r.tx_hash AND o.op_index = r.op_index
		ORDER BY o.ledger_seq, o.tx_hash, o.op_index
		SETTINGS join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 32`,
		from, to, opTypes, from, to)
}

// joinNoDedupeOpsSQL is the SHIPPED shape with the derived table's
// `GROUP BY tx_hash` removed — the correctness hazard IN's set semantics used
// to cover for free, and the reason the dedupe is not cosmetic.
func joinNoDedupeOpsSQL(opTypes string, from, to uint32) string {
	return fmt.Sprintf(`
		SELECT o.ledger_seq, o.close_time, o.tx_hash, o.op_index, o.source_account,
		       o.body_xdr, r.result_xdr
		FROM stellar.operations AS o
		INNER JOIN (
		    SELECT tx_hash FROM stellar.transactions
		    WHERE successful = 1 AND ledger_seq BETWEEN %d AND %d
		) AS t ON o.tx_hash = t.tx_hash
		INNER JOIN stellar.operation_results AS r
		  ON o.ledger_seq = r.ledger_seq AND o.tx_hash = r.tx_hash AND o.op_index = r.op_index
		WHERE o.ledger_seq BETWEEN %d AND %d
		  AND o.op_type IN (%s)
		ORDER BY o.ledger_seq, o.tx_hash, o.op_index
		SETTINGS join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 32`,
		from, to, from, to, opTypes)
}

// ── fixture ─────────────────────────────────────────────────────────────────

type opsPruneFixture struct {
	from, to    uint32
	wantSDEX    int // trade-typed ops in the window whose tx succeeded
	wantClassic int // payment-typed ops in the window whose tx succeeded
}

// seedOpsPruneFixture writes opsPruneRows ledgers of one op / one result / one
// tx each, alternating a trade op type and a classic one, with every
// opsPruneFailMod'th tx failed, plus a duplicate transactions part over the
// read window. Merges on stellar.transactions are stopped for the duration so
// that duplicate part is still there when the queries run.
func seedOpsPruneFixture(t *testing.T, ctx context.Context, raw driver.Conn) opsPruneFixture {
	t.Helper()
	const sourceAccount = "GTEST_OPSPRUNE_SOURCE_AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := raw.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %.90q: %v", q, err)
		}
	}
	mustExec(`SYSTEM STOP MERGES stellar.transactions`)
	// WithoutCancel, not the test's ctx: cleanups run after the test function
	// has returned and cancelled it, and leaving merges stopped would leak into
	// every later test sharing this container.
	restoreCtx := context.WithoutCancel(ctx)
	t.Cleanup(func() { _ = raw.Exec(restoreCtx, `SYSTEM START MERGES stellar.transactions`) })

	seq := fmt.Sprintf(`toUInt32(%d + number)`, opsPruneBase)
	hash := `lpad(toString(` + seq + `), 64, '0')`
	// Kept inside 2026/04 so the fixture never becomes the global close_time
	// tip another test anchors on (account_activity_watermark_test.go).
	closeAt := `toDateTime('2026-04-01 00:00:00', 'UTC') + toIntervalMillisecond(number)`
	isTrade := `number % 2 = 0`
	body := fmt.Sprintf(`if(%s, '%s', '%s')`, isTrade,
		opsPruneBodyB64(t, opsPruneSellOfferBody()), opsPruneBodyB64(t, opsPrunePaymentBody()))

	mustExec(fmt.Sprintf(`INSERT INTO stellar.operations
		(ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)
		SELECT %s, %s, %s, 0, 0, if(%s, '%s', '%s'), '%s', %s FROM numbers(%d)`,
		seq, closeAt, hash, isTrade, opsPruneSDEXType, opsPruneClassicType, sourceAccount, body, opsPruneRows))
	mustExec(fmt.Sprintf(`INSERT INTO stellar.operation_results
		(ledger_seq, tx_hash, op_index, result_code, result_xdr)
		SELECT %s, %s, 0, 0, '%s' FROM numbers(%d)`,
		seq, hash, opsPruneResultB64(t), opsPruneRows))
	insertTxs := func(where string) {
		mustExec(fmt.Sprintf(`INSERT INTO stellar.transactions
			(ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee,
			 operation_count, successful, result_code, memo_type, memo)
			SELECT %s, %s, %s, 0, '%s', 100, 200, 1, if(number %% %d = 0, 0, 1), 0, 'none', ''
			FROM numbers(%d) WHERE %s`,
			seq, closeAt, hash, sourceAccount, opsPruneFailMod, opsPruneRows, where))
	}
	insertTxs(`1`)
	insertTxs(fmt.Sprintf(`number < %d`, opsPruneDupRows))

	f := opsPruneFixture{from: opsPruneBase, to: opsPruneBase + opsPruneWindow - 1}
	for n := uint32(0); n < opsPruneWindow; n++ {
		if n%opsPruneFailMod == 0 {
			continue
		}
		if n%2 == 0 {
			f.wantSDEX++
		} else {
			f.wantClassic++
		}
	}
	if f.wantSDEX == 0 || f.wantClassic == 0 {
		t.Fatalf("fixture window yields no rows (sdex %d, classic %d)", f.wantSDEX, f.wantClassic)
	}
	return f
}

func opsPruneSellOfferBody() xdr.OperationBody {
	const issuerAccount = "GCEZWKCA5VLDNRLN3RPRJMRZOX3Z6G5CHCGSNFHEYVXM3XOJMDS674JZ"
	return xdr.OperationBody{
		Type: xdr.OperationTypeManageSellOffer,
		ManageSellOfferOp: &xdr.ManageSellOfferOp{
			Selling: xdr.MustNewNativeAsset(),
			Buying:  xdr.MustNewCreditAsset("USDC", issuerAccount),
			Amount:  xdr.Int64(1_000_0000),
			Price:   xdr.Price{N: 1, D: 2},
		},
	}
}

func opsPrunePaymentBody() xdr.OperationBody {
	const destAccount = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	return xdr.OperationBody{
		Type: xdr.OperationTypePayment,
		PaymentOp: &xdr.PaymentOp{
			Destination: xdr.MustMuxedAddress(destAccount),
			Asset:       xdr.MustNewNativeAsset(),
			Amount:      xdr.Int64(42_0000000),
		},
	}
}

func opsPruneBodyB64(t *testing.T, body xdr.OperationBody) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(body)
	if err != nil {
		t.Fatalf("marshal op body: %v", err)
	}
	return b64
}

func opsPruneResultB64(t *testing.T) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(xdr.OperationResult{Code: xdr.OperationResultCodeOpBadAuth})
	if err != nil {
		t.Fatalf("marshal op result: %v", err)
	}
	return b64
}

// ── plan + cost helpers ─────────────────────────────────────────────────────

type opsPruneKey struct {
	ledger  uint32
	txHash  string
	opIndex uint32
}

// explainIndexes returns `EXPLAIN indexes = 1` for sql, one plan line per
// element (indentation preserved — granulesFor walks the subtree by indent).
func explainIndexes(t *testing.T, ctx context.Context, raw driver.Conn, label, sql string) []string {
	t.Helper()
	rows, err := raw.Query(ctx, "EXPLAIN indexes = 1 "+sql)
	if err != nil {
		t.Fatalf("EXPLAIN %s: %v\n%s", label, err, sql)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN %s: %v", label, err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN %s rows: %v", label, err)
	}
	if len(plan) == 0 {
		t.Fatalf("EXPLAIN %s returned no plan", label)
	}
	return plan
}

func planHasStep(plan []string, step string) bool {
	for _, l := range plan {
		if strings.Contains(l, step) {
			return true
		}
	}
	return false
}

// granulesFor reads the LAST `Granules: read/total` of the ReadFromMergeTree
// node for `table` — the count after the final index step (Min-Max, then
// Partition, then PrimaryKey, then any skip index), i.e. what the query
// actually reads. ok is false when the node carries no index analysis at all,
// which is itself the unpruned verdict.
//
// The node's `Indexes:` block sits at the SAME indentation as the
// ReadFromMergeTree line, with its sections nested under it; the subtree walk
// has to allow that one sibling or it stops before reading anything.
func granulesFor(plan []string, table string) (read, total int, ok bool) {
	node := -1
	for i, l := range plan {
		if strings.Contains(l, "ReadFromMergeTree") && strings.Contains(l, table) {
			node = i
			break
		}
	}
	if node < 0 {
		return 0, 0, false
	}
	indent := len(plan[node]) - len(strings.TrimLeft(plan[node], " "))
	for _, l := range plan[node+1:] {
		body := strings.TrimLeft(l, " ")
		if body == "" {
			continue
		}
		switch depth := len(l) - len(body); {
		case depth < indent, depth == indent && body != "Indexes:":
			return read, total, ok
		}
		var r, tot int
		if _, err := fmt.Sscanf(body, "Granules: %d/%d", &r, &tot); err == nil {
			read, total, ok = r, tot, true
		}
	}
	return read, total, ok
}

// requirePrunedOperationResults is the assertion the previous remediation
// failed: the ledger window must still reach stellar.operation_results through
// the join key, so the read is a primary-key range and not the whole table.
func requirePrunedOperationResults(t *testing.T, label string, plan []string) {
	t.Helper()
	read, total, ok := granulesFor(plan, "operation_results")
	if !ok {
		t.Fatalf("%s: stellar.operation_results is read with NO primary-key index analysis — "+
			"the ledger window is not propagated into the join's build side, so it full-scans "+
			"full chain history of wide result_xdr:\n%s", label, strings.Join(plan, "\n"))
	}
	if read >= total {
		t.Fatalf("%s: stellar.operation_results reads ALL %d/%d granules — primary-key pruning lost:\n%s",
			label, read, total, strings.Join(plan, "\n"))
	}
	t.Logf("%s: operation_results granules %d/%d (pruned)", label, read, total)
}

// oracleKeys runs a frozen oracle query and returns its emitted keys.
func oracleKeys(t *testing.T, ctx context.Context, raw driver.Conn, label, sql string) []opsPruneKey {
	t.Helper()
	rows, err := raw.Query(ctx, sql)
	if err != nil {
		t.Fatalf("%s: %v\n%s", label, err, sql)
	}
	defer func() { _ = rows.Close() }()
	var out []opsPruneKey
	for rows.Next() {
		var (
			k                    opsPruneKey
			closeTime            time.Time
			source, body, result string
		)
		if err := rows.Scan(&k.ledger, &closeTime, &k.txHash, &k.opIndex, &source, &body, &result); err != nil {
			t.Fatalf("%s scan: %v", label, err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s rows: %v", label, err)
	}
	return out
}

// readRowsOf runs sql under a fresh query id and reports its read_rows.
func readRowsOf(t *testing.T, ctx context.Context, raw driver.Conn, label, sql string) uint64 {
	t.Helper()
	id := uuid.NewString()
	_ = oracleKeys(t, clickhouse.Context(ctx, clickhouse.WithQueryID(id)), raw, label, sql)
	return queryLogReadRows(t, ctx, raw, label, id)
}

func queryLogReadRows(t *testing.T, ctx context.Context, raw driver.Conn, label, queryID string) uint64 {
	t.Helper()
	if err := raw.Exec(ctx, `SYSTEM FLUSH LOGS`); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	var rr uint64
	if err := raw.QueryRow(ctx, `SELECT read_rows FROM system.query_log
		WHERE query_id = ? AND type = 'QueryFinish'
		ORDER BY event_time_microseconds DESC LIMIT 1`, queryID).Scan(&rr); err != nil {
		t.Fatalf("query_log read_rows (%s): %v", label, err)
	}
	return rr
}

// captureShippedQuery drives a reader through its PRODUCTION entry point and
// recovers the exact SQL ClickHouse executed (the driver binds client-side, so
// system.query_log holds the statement with its literals) plus its read_rows.
func captureShippedQuery(t *testing.T, ctx context.Context, raw driver.Conn,
	label string, stream func(context.Context) error,
) (sql string, readRows uint64) {
	t.Helper()
	id := uuid.NewString()
	if err := stream(clickhouse.Context(ctx, clickhouse.WithQueryID(id))); err != nil {
		t.Fatalf("%s stream: %v", label, err)
	}
	if err := raw.Exec(ctx, `SYSTEM FLUSH LOGS`); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	if err := raw.QueryRow(ctx, `SELECT query, read_rows FROM system.query_log
		WHERE query_id = ? AND type = 'QueryFinish'
		ORDER BY event_time_microseconds DESC LIMIT 1`, id).Scan(&sql, &readRows); err != nil {
		t.Fatalf("query_log (%s): %v", label, err)
	}
	return sql, readRows
}

// opTypeInListFrom lifts the op-type IN list out of the shipped statement, so
// the frozen oracles filter on EXACTLY what the reader filtered on and any row
// difference between them is attributable to the query SHAPE alone.
func opTypeInListFrom(t *testing.T, label, sql string) string {
	t.Helper()
	// Unqualified, so a reshaped statement that spells the filter inside a
	// derived table still reaches the pruning verdict below rather than dying
	// here on a cosmetic alias change.
	const marker = "op_type IN ("
	i := strings.Index(sql, marker)
	if i < 0 {
		t.Fatalf("%s: shipped statement has no op-type filter:\n%s", label, sql)
	}
	rest := sql[i+len(marker):]
	j := strings.Index(rest, ")")
	if j < 0 {
		t.Fatalf("%s: unterminated op-type filter:\n%s", label, sql)
	}
	return rest[:j]
}

// ── the test ────────────────────────────────────────────────────────────────

// TestStreamOpsQueriesPruneOperationResults is the live proof.
func TestStreamOpsQueriesPruneOperationResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")
	f := seedOpsPruneFixture(t, ctx, raw)

	var sdexKeys []opsPruneKey
	sdexSQL, sdexRead := captureShippedQuery(t, ctx, raw, "sdex", func(qctx context.Context) error {
		sdexKeys = nil
		return chstore.StreamSDEXOps(qctx, addr, f.from, f.to, func(op chstore.SDEXOp) error {
			sdexKeys = append(sdexKeys, opsPruneKey{op.Ledger, op.TxHash, op.OpIndex})
			return nil
		})
	})
	assertOpStreamBounded(t, ctx, raw, f, "sdex", sdexSQL, sdexRead, sdexKeys, f.wantSDEX)

	var classicKeys []opsPruneKey
	classicSQL, classicRead := captureShippedQuery(t, ctx, raw, "classic", func(qctx context.Context) error {
		classicKeys = nil
		return chstore.StreamClassicOps(qctx, addr, f.from, f.to,
			[]string{opsPruneClassicType}, func(op chstore.ClassicOp) error {
				classicKeys = append(classicKeys, opsPruneKey{op.Ledger, op.TxHash, op.OpIndex})
				return nil
			})
	})
	assertOpStreamBounded(t, ctx, raw, f, "classic", classicSQL, classicRead, classicKeys, f.wantClassic)
}

// assertOpStreamBounded pins, for one shipped op-stream statement: no
// successful-tx set-build, primary-key pruning retained on
// stellar.operation_results (the rejected remediation), the exact rows the
// IN-subquery shape served (no over- or under-count from the join), and a
// read_rows bound. Each assertion is paired with a non-vacuity guard against
// the frozen oracle that is supposed to violate it.
func assertOpStreamBounded(t *testing.T, ctx context.Context, raw driver.Conn, f opsPruneFixture,
	name, shippedSQL string, shippedRead uint64, got []opsPruneKey, want int,
) {
	t.Helper()
	opTypes := opTypeInListFrom(t, name, shippedSQL)
	legacySQL := inSubqueryOpsSQL(opTypes, f.from, f.to)
	rejectedSQL := derivedWindowOpsSQL(opTypes, f.from, f.to)

	// (1) The defect: the successful-tx set must not be materialised.
	plan := explainIndexes(t, ctx, raw, name+" shipped", shippedSQL)
	if planHasStep(plan, "CreatingSet") {
		t.Errorf("%s: the shipped statement still plans a CreatingSet — the window's whole "+
			"successful-tx hash set is materialised before the join (the 10 GiB blowout of "+
			"2026-07-11):\n%s", name, strings.Join(plan, "\n"))
	}
	if !planHasStep(explainIndexes(t, ctx, raw, name+" in-subquery oracle", legacySQL), "CreatingSet") {
		t.Fatalf("%s: the frozen IN-subquery oracle no longer plans a CreatingSet against this "+
			"fixture — the assertion above cannot fail and is vacuous", name)
	}

	// (2) The rejected remediation: pruning must survive the reshape.
	requirePrunedOperationResults(t, name+" shipped", plan)
	rejectedPlan := explainIndexes(t, ctx, raw, name+" derived-window oracle", rejectedSQL)
	if read, total, ok := granulesFor(rejectedPlan, "operation_results"); ok && read < total {
		t.Fatalf("%s: the frozen derived-window oracle still prunes operation_results (%d/%d "+
			"granules) — the pruning assertion above cannot fail and is vacuous", name, read, total)
	}

	// (3) Rows: exactly what the IN-subquery shape served, and no more.
	wantKeys := oracleKeys(t, ctx, raw, name+" in-subquery oracle", legacySQL)
	if len(got) != len(wantKeys) || len(got) != want {
		t.Fatalf("%s: reader served %d rows, IN-subquery oracle %d, fixture expects %d",
			name, len(got), len(wantKeys), want)
	}
	for i := range got {
		if got[i] != wantKeys[i] {
			t.Fatalf("%s row %d differs: reader %+v, IN-subquery oracle %+v", name, i, got[i], wantKeys[i])
		}
	}
	if fanned := oracleKeys(t, ctx, raw, name+" no-dedupe oracle",
		joinNoDedupeOpsSQL(opTypes, f.from, f.to)); len(fanned) <= len(got) {
		t.Fatalf("%s: dropping GROUP BY tx_hash served %d rows, not more than the shipped %d — "+
			"the duplicate transactions part no longer fans out, so the dedupe is unproven",
			name, len(fanned), len(got))
	}

	// (4) Cost: the shipped read stays a window read.
	rejectedRead := readRowsOf(t, ctx, raw, name+" derived-window oracle", rejectedSQL)
	t.Logf("%s read_rows: shipped=%d derived-window=%d (window %d ledgers of %d)",
		name, shippedRead, rejectedRead, opsPruneWindow, opsPruneRows)
	if rejectedRead < opsPruneRows {
		t.Fatalf("%s: the derived-window oracle read only %d rows — it is not the unbounded scan "+
			"this bound is calibrated against", name, rejectedRead)
	}
	if shippedRead*4 > rejectedRead {
		t.Fatalf("%s: the shipped statement read %d rows against the unpruned shape's %d — the read "+
			"must be bounded by the ledger window, not by chain history", name, shippedRead, rejectedRead)
	}
}

// cap76Checkpoint is the first checkpoint after the CAP-0076 upgrade ledger.
const cap76Checkpoint = 59_501_311

// resetNetworkStateLake empties the tables these tests read, before and after,
// so other tests' fixture rows cannot enter the lumen sum or the lake tip.
func resetNetworkStateLake(t *testing.T, ctx context.Context) {
	t.Helper()
	raw := dialClickHouse(t, ctx, "stellar")
	truncate := func() {
		for _, table := range []string{"ledgers", "ledger_entry_changes", "ledger_entries_current"} {
			if err := raw.Exec(context.Background(), "TRUNCATE TABLE stellar."+table); err != nil {
				t.Errorf("truncate %s: %v", table, err)
			}
		}
	}
	truncate()
	t.Cleanup(truncate)
}

func insertNetworkStateLedger(t *testing.T, ctx context.Context, seq uint32, totalCoins, feePool int64) {
	t.Helper()
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, `INSERT INTO stellar.ledgers
		(ledger_seq, close_time, ledger_hash, prev_hash, protocol_version, total_coins, fee_pool)
		VALUES (?, ?, ?, '00', 24, ?, ?)`,
		seq, time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC), fmt.Sprintf("%064d", seq), totalCoins, feePool); err != nil {
		t.Fatalf("insert ledger %d: %v", seq, err)
	}
}

// entryChange builds a ledger_entry_changes row in the extractor's encoding.
func entryChange(t *testing.T, seq, intra uint32, changeType string, e xdr.LedgerEntry) chstore.LedgerEntryChangeRow {
	t.Helper()
	k, err := e.LedgerKey()
	if err != nil {
		t.Fatalf("ledger key: %v", err)
	}
	entryType, key, err := chstore.LedgerKeyColumns(k)
	if err != nil {
		t.Fatal(err)
	}
	row := chstore.LedgerEntryChangeRow{
		LedgerSeq: seq, CloseTime: time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC), TxHash: "network-state",
		ChangeIndex: intra, IntraLedgerSeq: intra, ChangeType: changeType, EntryType: entryType, KeyXDR: key,
	}
	if changeType == "removed" {
		return row
	}
	if row.EntryXDR, err = xdr.MarshalBase64(e); err != nil {
		t.Fatal(err)
	}
	if a, ok := e.Data.GetAccount(); ok {
		row.Balance = int64(a.Balance)
	}
	return row
}

func insertEntryChanges(t *testing.T, ctx context.Context, rows ...chstore.LedgerEntryChangeRow) {
	t.Helper()
	if _, err := chstore.InsertEntryChanges(ctx, clickhouseAddr(t), rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
}

func cap76Data(name string, val uint64) xdr.LedgerEntry {
	id := xdr.ContractId{0xca, 0x76, 0x1e}
	sym := xdr.ScSymbol(name)
	v := xdr.Uint64(val)
	return xdr.LedgerEntry{
		LastModifiedLedgerSeq: 59_000_000,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id},
				Key:        xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v},
			},
		},
	}
}

// writeHotArchiveFixture lays out a file:// history archive whose checkpoint
// HAS carries one hot-archive bucket holding entries.
func writeHotArchiveFixture(t *testing.T, entries ...xdr.LedgerEntry) string {
	t.Helper()
	dir := t.TempDir()
	listType := xdr.BucketListTypeHotArchive
	var raw bytes.Buffer
	if err := xdr.MarshalFramed(&raw, xdr.HotArchiveBucketEntry{
		Type:      xdr.HotArchiveBucketEntryTypeHotArchiveMetaentry,
		MetaEntry: &xdr.BucketMetadata{LedgerVersion: 24, Ext: xdr.BucketMetadataExt{V: 1, BucketListType: &listType}},
	}); err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		if err := xdr.MarshalFramed(&raw, xdr.HotArchiveBucketEntry{
			Type: xdr.HotArchiveBucketEntryTypeHotArchiveArchived, ArchivedEntry: &entries[i],
		}); err != nil {
			t.Fatal(err)
		}
	}
	hash := historyarchive.Hash(sha256.Sum256(raw.Bytes()))
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(dir, historyarchive.BucketPath(hash)), gz.Bytes())

	has := historyarchive.HistoryArchiveState{Version: 2, CurrentLedger: cap76Checkpoint, NetworkPassphrase: network.PublicNetworkPassphrase}
	zero := strings.Repeat("0", 64)
	for i := range has.HotArchiveBuckets {
		has.CurrentBuckets[i].Curr, has.CurrentBuckets[i].Snap = zero, zero
		has.HotArchiveBuckets[i].Curr, has.HotArchiveBuckets[i].Snap = zero, zero
	}
	has.HotArchiveBuckets[0].Curr = hex.EncodeToString(hash[:])
	body, err := json.Marshal(has)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(dir, historyarchive.CategoryCheckpointPath("history", cap76Checkpoint)), body)
	writeFixtureFile(t, filepath.Join(dir, ".well-known", "stellar-history.json"), body)
	return "file://" + dir
}

func writeFixtureFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyNetworkState_CAP76AmendmentFailsOnAmendedKeys replays the
// CAP-0076 case end to end: the lake holds archived entries as last seen in
// ledger meta, the history archive's hot-archive bucket holds two of them
// amended. The verb must exit with exactly the two mismatches; an archive
// matching the lake must pass.
func TestVerifyNetworkState_CAP76AmendmentFailsOnAmendedKeys(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	resetNetworkStateLake(t, ctx)
	insertNetworkStateLedger(t, ctx, cap76Checkpoint, 0, 0)
	const evicted = 59_000_000
	insertEntryChanges(t, ctx,
		entryChange(t, evicted, 1, "updated", cap76Data("amended_a", 1)),
		entryChange(t, evicted, 2, "updated", cap76Data("amended_b", 2)),
		entryChange(t, evicted, 3, "updated", cap76Data("untouched", 3)),
	)

	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	writeFixtureFile(t, cfgPath, []byte("[stellar]\nnetwork = \"pubnet\"\n"))
	run := func(archiveURL, textfile string) error {
		return chops.Run([]string{
			"verify-network-state", "-config", cfgPath, "-ch-addr", addr, "-archive", archiveURL,
			"-checks", "hotarchive", "-checkpoint", fmt.Sprint(cap76Checkpoint), "-textfile", textfile,
		})
	}

	amended := writeHotArchiveFixture(t, cap76Data("amended_a", 101), cap76Data("amended_b", 102), cap76Data("untouched", 3))
	prom := filepath.Join(t.TempDir(), "network_state_verify.prom")
	var exit *opsutil.ExitCodeError
	if err := run(amended, prom); !errors.As(err, &exit) || exit.Code != 2 {
		t.Fatalf("amended archive: err = %v, want exit code 2 (the two amended keys)", err)
	}
	body, err := os.ReadFile(prom)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`stellarindex_network_state_verify_failures{check="hot_archive"} 2`,
		`stellarindex_network_state_verify_hot_archive_entries{class="matched"} 1`,
		`stellarindex_network_state_verify_hot_archive_entries{class="mismatched"} 2`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("textfile missing %q:\n%s", want, body)
		}
	}

	clean := writeHotArchiveFixture(t, cap76Data("amended_a", 1), cap76Data("amended_b", 2), cap76Data("untouched", 3))
	if err := run(clean, filepath.Join(t.TempDir(), "clean.prom")); err != nil {
		t.Fatalf("archive matching the lake: %v", err)
	}
}

func lumenAccount(seed byte, balance int64) xdr.LedgerEntry {
	var pk xdr.Uint256
	pk[0] = seed
	id := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type:    xdr.LedgerEntryTypeAccount,
		Account: &xdr.AccountEntry{AccountId: id, Balance: xdr.Int64(balance)},
	}}
}

func lumenCB(seed byte, asset xdr.Asset, amount int64) xdr.LedgerEntry {
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeClaimableBalance,
		ClaimableBalance: &xdr.ClaimableBalanceEntry{
			BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &xdr.Hash{seed}},
			Asset:     asset,
			Amount:    xdr.Int64(amount),
		},
	}}
}

func lumenLP(native, credit xdr.Asset, reserveNative int64) xdr.LedgerEntry {
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeLiquidityPool,
		LiquidityPool: &xdr.LiquidityPoolEntry{
			LiquidityPoolId: xdr.PoolId{0x1f},
			Body: xdr.LiquidityPoolEntryBody{
				Type: xdr.LiquidityPoolTypeLiquidityPoolConstantProduct,
				ConstantProduct: &xdr.LiquidityPoolEntryConstantProduct{
					Params:   xdr.LiquidityPoolConstantProductParameters{AssetA: native, AssetB: credit, Fee: 30},
					ReserveA: xdr.Int64(reserveNative), ReserveB: 999_999,
				},
			},
		},
	}}
}

func lumenSACBalance(sac xdr.ContractId, holder byte, amount int64) xdr.LedgerEntry {
	sym := xdr.ScSymbol("Balance")
	var pk xdr.Uint256
	pk[0] = holder
	acct := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}
	vec := xdr.ScVec{
		{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
		{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &acct}},
	}
	pv := &vec
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &sac},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Lo: xdr.Uint64(amount)}},
		},
	}}
}

// TestNetworkStateReader_LumenConservationAtCommittedTip proves the lumen
// tally against real ClickHouse: it sums every native holding domain as of
// the newest stellar.ledgers row and rolls back changes the sink wrote for a
// ledger whose header is not yet committed.
func TestNetworkStateReader_LumenConservationAtCommittedTip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	resetNetworkStateLake(t, ctx)

	sac := xdr.ContractId{0x5a, 0xc0}
	sacID, err := strkey.Encode(strkey.VersionByteContract, sac[:])
	if err != nil {
		t.Fatal(err)
	}
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	credit := xdr.MustNewCreditAsset("USDC", "GC3C4AKRBQLHOJ45U4XG35ESVWRDECWO5XLDGYADO6DPR3L7KIDVUMML")

	const tip = 100
	insertNetworkStateLedger(t, ctx, tip-1, 1_000, 200)
	insertNetworkStateLedger(t, ctx, tip, 1_000, 220)
	insertEntryChanges(t, ctx,
		entryChange(t, 90, 1, "created", lumenAccount(1, 600)),
		entryChange(t, 91, 1, "created", lumenAccount(3, 7)),
		entryChange(t, 99, 1, "removed", lumenAccount(3, 0)),
		entryChange(t, 95, 1, "created", lumenCB(1, native, 100)),
		entryChange(t, 95, 2, "created", lumenCB(2, credit, 5_000)),
		entryChange(t, 95, 3, "created", lumenLP(native, credit, 30)),
		entryChange(t, 95, 4, "created", lumenSACBalance(sac, 1, 50)),
		entryChange(t, 95, 5, "created", lumenSACBalance(xdr.ContractId{0x5a, 0xc1}, 1, 9_999)),
		// Ledger 101's changes are in the lake but its header is not.
		entryChange(t, tip+1, 1, "state", lumenAccount(1, 600)),
		entryChange(t, tip+1, 2, "updated", lumenAccount(1, 400)),
		entryChange(t, tip+1, 3, "created", lumenAccount(2, 200)),
		entryChange(t, tip+1, 4, "state", lumenCB(1, native, 100)),
		entryChange(t, tip+1, 5, "removed", lumenCB(1, native, 0)),
	)

	reader, err := chstore.NewNetworkStateReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	got, err := reader.LumenConservation(ctx, sacID)
	if err != nil {
		t.Fatalf("LumenConservation: %v", err)
	}
	if got.Ledger != tip || got.TotalCoins != 1_000 || got.FeePool != 220 || got.Preimages != 3 {
		t.Fatalf("header/preimages = (%d, %d, %d, %d), want (%d, 1000, 220, 3)", got.Ledger, got.TotalCoins, got.FeePool, got.Preimages, tip)
	}
	for _, c := range []struct {
		name string
		got  *big.Int
		want int64
	}{
		{"accounts", got.Accounts, 600},
		{"claimable_balances", got.ClaimableBalances, 100},
		{"liquidity_pools", got.LiquidityPools, 30},
		{"contract_balances", got.ContractBalances, 50},
	} {
		if c.got.Cmp(big.NewInt(c.want)) != 0 {
			t.Errorf("%s = %s, want %d", c.name, c.got, c.want)
		}
	}
	if r := got.Residual(); r.Sign() != 0 {
		t.Errorf("residual = %s, want 0", r)
	}

	// The same lake under a header claiming 5 more stroops exist must not conserve.
	insertNetworkStateLedger(t, ctx, tip, 1_005, 220)
	got, err = reader.LumenConservation(ctx, sacID)
	if err != nil {
		t.Fatalf("LumenConservation: %v", err)
	}
	if r := got.Residual(); r.Cmp(big.NewInt(-5)) != 0 {
		t.Fatalf("residual = %s, want -5", r)
	}
}

func lumenSACAllowance(sac xdr.ContractId, amount int64) xdr.LedgerEntry {
	sym := xdr.ScSymbol("Allowance")
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &sac},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
			Durability: xdr.ContractDataDurabilityTemporary,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Lo: xdr.Uint64(amount)}},
		},
	}}
}

// evictionRow is the lake walker's eviction row: a `removed` with no tx and no
// preceding `state`, so it has no pre-image in ledger_entry_changes.
func evictionRow(t *testing.T, seq, intra uint32, e xdr.LedgerEntry) chstore.LedgerEntryChangeRow {
	row := entryChange(t, seq, intra, "removed", e)
	row.TxHash, row.OpIndex, row.ChangeIndex = "", -1, 0
	return row
}

// TestNetworkStateReader_LumenConservationEvictionPastTip: a native SAC
// allowance evicted in a ledger whose header is not yet committed holds no
// lumens and must not abort the tally; an evicted balance with no pre-image
// still fails loud, since its lumens would otherwise vanish from the sum.
func TestNetworkStateReader_LumenConservationEvictionPastTip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	resetNetworkStateLake(t, ctx)

	sac := xdr.ContractId{0x5a, 0xc0}
	sacID, err := strkey.Encode(strkey.VersionByteContract, sac[:])
	if err != nil {
		t.Fatal(err)
	}
	const tip = 100
	insertNetworkStateLedger(t, ctx, tip, 1_050, 0)
	insertEntryChanges(t, ctx,
		entryChange(t, 90, 1, "created", lumenAccount(1, 1_000)),
		entryChange(t, 95, 1, "created", lumenSACBalance(sac, 1, 50)),
		entryChange(t, 95, 2, "created", lumenSACAllowance(sac, 7_777)),
		evictionRow(t, tip+1, 1, lumenSACAllowance(sac, 0)),
	)

	reader, err := chstore.NewNetworkStateReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	got, err := reader.LumenConservation(ctx, sacID)
	if err != nil {
		t.Fatalf("LumenConservation with an allowance evicted past the tip: %v", err)
	}
	if got.Ledger != tip || got.Preimages != 0 || got.ContractBalances.Cmp(big.NewInt(50)) != 0 || got.Residual().Sign() != 0 {
		t.Fatalf("tally = ledger %d, preimages %d, contract balances %s, residual %s; want %d, 0, 50, 0",
			got.Ledger, got.Preimages, got.ContractBalances, got.Residual(), tip)
	}

	insertEntryChanges(t, ctx, evictionRow(t, tip+1, 2, lumenSACBalance(sac, 1, 0)))
	if _, err := reader.LumenConservation(ctx, sacID); err == nil || !strings.Contains(err.Error(), "no pre-image") {
		t.Fatalf("balance removed past the tip with no pre-image: err = %v, want a no-pre-image error", err)
	}
}

func TestNetworkStateReader_CurrentEntries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	resetNetworkStateLake(t, ctx)
	live := entryChange(t, 96, 1, "updated", cap76Data("live", 2))
	gone := entryChange(t, 97, 1, "removed", cap76Data("gone", 0))
	insertEntryChanges(t, ctx,
		entryChange(t, 95, 1, "created", cap76Data("live", 1)),
		live,
		entryChange(t, 90, 1, "created", cap76Data("gone", 1)),
		gone,
	)
	reader, err := chstore.NewNetworkStateReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	got, err := reader.CurrentEntries(ctx, "contract_data", []string{live.KeyXDR, gone.KeyXDR, "absent-key"})
	if err != nil {
		t.Fatalf("CurrentEntries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	if e := got[live.KeyXDR]; e.LedgerSeq != 96 || e.ChangeType != "updated" || e.EntryXDR != live.EntryXDR {
		t.Errorf("live key resolved to %+v, want the ledger-96 update", e)
	}
	if e := got[gone.KeyXDR]; e.LedgerSeq != 97 || e.ChangeType != "removed" {
		t.Errorf("removed key resolved to %+v, want the ledger-97 removal", e)
	}
}
