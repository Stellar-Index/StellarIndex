//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

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
