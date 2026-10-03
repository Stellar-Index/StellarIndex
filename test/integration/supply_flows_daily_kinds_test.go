//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestDailySupplyFlowsPerKindSplit executes the daily supply-flows query
// against real ClickHouse: per-kind sums beside the net, i128 amounts above
// 2^63 intact, and a re-inserted flow collapsed by FINAL.
func TestDailySupplyFlowsPerKindSplit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const contract = "CDAILYKINDSPLITXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
	const insert = `INSERT INTO stellar.supply_flows
		(contract_id, ledger_seq, close_time, tx_hash, op_index, event_index, kind, amount)
		VALUES (?, ?, ?, ?, 0, ?, ?, toInt128(?))`
	day1 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 9, 3, 23, 59, 0, 0, time.UTC)
	rows := []struct {
		ledger uint32
		at     time.Time
		tx     string
		ev     uint32
		kind   string
		amount string
	}{
		{80_000_001, day1, "a1", 0, "mint", "18446744073709551621"}, // 2^64 + 5
		{80_000_001, day1, "a1", 1, "burn", "21"},
		{80_000_002, day1, "a2", 0, "clawback", "4"},
		{80_050_000, day2, "b1", 0, "mint", "100"},
		{80_050_000, day2, "b1", 0, "mint", "100"}, // same flow identity: FINAL collapses it
	}
	for _, r := range rows {
		if err := conn.Exec(ctx, insert, contract, r.ledger, r.at, r.tx, r.ev, r.kind, r.amount); err != nil {
			t.Fatalf("insert %s: %v", r.kind, err)
		}
	}

	sr, err := chstore.NewSupplyReader(ctx, addr)
	if err != nil {
		t.Fatalf("new supply reader: %v", err)
	}
	t.Cleanup(func() { _ = sr.Close() })
	got, err := sr.DailySupplyFlowsForContracts(ctx, []string{contract})
	if err != nil {
		t.Fatalf("DailySupplyFlowsForContracts: %v", err)
	}
	type day struct{ date, mint, burn, clawback, net string }
	want := []day{
		{"2026-09-01", "18446744073709551621", "21", "4", "18446744073709551596"},
		{"2026-09-03", "100", "0", "0", "100"},
	}
	wantFlows := []uint64{3, 1}
	if len(got) != len(want) {
		t.Fatalf("got %d days, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		gd := day{g.Day.Format(time.DateOnly), g.Mint.String(), g.Burn.String(), g.Clawback.String(), g.Net.String()}
		if gd != w || g.Flows != wantFlows[i] {
			t.Errorf("day %d = %+v flows=%d, want %+v flows=%d", i, gd, g.Flows, w, wantFlows[i])
		}
	}
}
