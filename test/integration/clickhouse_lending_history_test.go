//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestLendingReserveHistory_FinalSeedBucketsAndWatermark reads lending_state_hourly through the reader: FINAL collapses a
// duplicated hour, the seed carries the state in force at from, a day keeps its last hour and the watermark hides later rows.
func TestLendingReserveHistory_FinalSeedBucketsAndWatermark(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	for _, q := range []string{`TRUNCATE TABLE stellar.entry_history_watermark`, `TRUNCATE TABLE stellar.lending_state_hourly`} {
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	var poolID, assetID xdr.ContractId
	poolID[0], assetID[0] = 0x71, 0x72
	pool := strkey.MustEncode(strkey.VersionByteContract, poolID[:])
	asset := strkey.MustEncode(strkey.VersionByteContract, assetID[:])
	key := resDataKeyB64(t, poolID, assetID)
	day := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	q := chstore.LendingHistoryQuery{From: day.Add(9 * time.Hour), To: day.Add(48 * time.Hour), MaxPoints: 2000}

	if _, err := reader.LendingReserveHistory(ctx, pool, asset, q); !errors.Is(err, chstore.ErrLendingHistoryNotReady) {
		t.Fatalf("before any derive: err = %v, want not ready", err)
	}
	if err := conn.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES ('lending_state', 1000)`); err != nil {
		t.Fatal(err)
	}
	const base = 1_000_000_000_000
	for _, r := range []struct {
		closeAt time.Duration
		ledger  uint32
		bRate   uint64
	}{
		{8*time.Hour + 30*time.Minute, 100, base + 1},  // seed
		{10*time.Hour + 5*time.Minute, 190, base + 99}, // replaced by ledger 200 in the same hour
		{10*time.Hour + 20*time.Minute, 200, base + 2},
		{13*time.Hour + 10*time.Minute, 300, base + 3},
		{33 * time.Hour, 400, base + 4},
		{35 * time.Hour, 1500, base + 5}, // above the watermark
	} {
		at := day.Add(r.closeAt)
		if err := conn.Exec(ctx, `INSERT INTO stellar.lending_state_hourly
			(contract_id, protocol, key_xdr, hour, ledger_seq, intra_ledger_seq, close_time, entry_xdr)
			VALUES (?, 'blend_v2', ?, toStartOfHour(toDateTime(?, 'UTC')), ?, 1, toDateTime(?, 'UTC'), ?)`,
			pool, key, at.Unix(), r.ledger, at.Unix(), resDataEntryB64(t, poolID, assetID, r.ledger, r.bRate)); err != nil {
			t.Fatal(err)
		}
	}
	ledgers := func(h chstore.LendingReserveHistory) []uint32 {
		var out []uint32
		for _, p := range h.Points {
			out = append(out, p.StateLedger)
		}
		return out
	}

	hourly, err := reader.LendingReserveHistory(ctx, pool, asset, q)
	if err != nil {
		t.Fatal(err)
	}
	if got := ledgers(hourly); len(got) != 4 || got[0] != 100 || got[1] != 200 || got[2] != 300 || got[3] != 400 || hourly.BeforeFirstState {
		t.Fatalf("hourly points = %v (before_first_state %v)", got, hourly.BeforeFirstState)
	}
	if b := hourly.Points[1].State.Data.BRate.Uint64(); b != base+2 || hourly.Coverage.ThroughLedger != 1000 || hourly.Protocol != "blend_v2" {
		t.Fatalf("hour 10:00 b_rate = %d (FINAL must keep ledger 200), coverage %+v", b, hourly.Coverage)
	}

	q.Daily = true
	daily, err := reader.LendingReserveHistory(ctx, pool, asset, q)
	if err != nil {
		t.Fatal(err)
	}
	if got := ledgers(daily); len(got) != 3 || got[0] != 100 || got[1] != 300 || got[2] != 400 {
		t.Fatalf("daily points = %v, want seed then each day's last hour [100 300 400]", got)
	}

	q.Daily, q.MaxPoints = false, 2
	if _, err := reader.LendingReserveHistory(ctx, pool, asset, q); !errors.Is(err, chstore.ErrLendingHistoryTooManyPoints) {
		t.Fatalf("3 points over a cap of 2: err = %v", err)
	}
	q.From, q.MaxPoints = day.Add(-24*time.Hour), 2000
	early, err := reader.LendingReserveHistory(ctx, pool, asset, q)
	if err != nil || !early.BeforeFirstState || len(early.Points) != 4 || early.Points[0].StateLedger != 100 {
		t.Fatalf("from before the first state: %+v, %v", early, err)
	}
}
