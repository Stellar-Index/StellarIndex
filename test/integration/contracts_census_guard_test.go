//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

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
