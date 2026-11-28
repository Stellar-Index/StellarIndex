//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestLendingRateDays_BucketBoundsSlackPriorAndWatermark reads one reserve day by day: each day's first and last state after
// FINAL collapses an hour, the day before the range only through its last hour (the base slack), a day with no state
// absent, a state older than the slack never read but seen by the prior probe, and the watermark's hour withheld.
func TestLendingRateDays_BucketBoundsSlackPriorAndWatermark(t *testing.T) {
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
	poolID[0], assetID[0] = 0x91, 0x92
	enc := func(id xdr.ContractId) string { return strkey.MustEncode(strkey.VersionByteContract, id[:]) }
	asset := enc(assetID)
	day := time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)
	c := lendingHistoryCase{t: t, ctx: ctx, conn: conn, reader: reader, pool: enc(poolID), day: day}
	const base = 1_000_000_000_000
	put := func(ledger uint32, at time.Duration, bRate uint64) {
		c.insert(resDataKeyB64(t, poolID, assetID), ledger, at, resDataRatesEntryB64(t, poolID, assetID, ledger, bRate, 2*bRate, uint64(day.Add(at).Unix())))
	}
	c.setWatermark(rdL0+1000, day.Add(3*24*time.Hour+10*time.Hour+30*time.Minute))
	put(rdL0+10, -36*time.Hour, base)                               // day -2, 12:00: before every slack
	put(rdL0+20, -30*time.Minute, base+1)                           // day -1, 23:30: day 0's base slack
	put(rdL0+30, 3*time.Hour, base+2)                               // day 0, 03:00
	put(rdL0+40, 15*time.Hour+10*time.Minute, base+4)               // day 0, 15:10: the hour's last change
	put(rdL0+39, 15*time.Hour+5*time.Minute, base+99)               // the hour's lower version, inserted last
	put(rdL0+60, 2*24*time.Hour+6*time.Hour, base+6)                // day 2; day 1 has no state
	put(rdL0+65, 3*24*time.Hour+9*time.Hour, base+7)                // day 3, before the watermark's hour
	put(rdL0+70, 3*24*time.Hour+10*time.Hour+5*time.Minute, base+8) // the watermark's own hour

	cov, err := reader.LendingHistoryCoverage(ctx)
	if err != nil || cov.ThroughLedger != rdL0+1000 {
		t.Fatalf("coverage %+v: %v", cov, err)
	}
	read := func(from, to time.Time) chstore.LendingRateDays {
		t.Helper()
		d, err := reader.LendingRateDays(ctx, c.pool, asset, from, to, cov)
		if err != nil {
			t.Fatalf("LendingRateDays %s..%s: %v", from, to, err)
		}
		return d
	}
	want := func(got chstore.LendingRateDays, rows [][3]uint32) {
		t.Helper()
		if len(got.Days) != len(rows) {
			t.Fatalf("days %+v, want %d", got.Days, len(rows))
		}
		for i, r := range rows {
			d := got.Days[i]
			if !d.Day.Equal(day.AddDate(0, 0, int(r[0])-1)) || d.First == nil || d.Last == nil ||
				d.First.Ledger != rdL0+r[1] || d.Last.Ledger != rdL0+r[2] {
				t.Fatalf("day %d = %+v first %+v last %+v, want day %d ledgers %d..%d", i, d, d.First, d.Last, int(r[0])-1, r[1], r[2])
			}
		}
	}

	all := read(day, day.AddDate(0, 0, 4))
	want(all, [][3]uint32{{0, 20, 20}, {1, 30, 40}, {3, 60, 60}, {4, 65, 65}})
	if all.Days[1].Last.BRate.Uint64() != base+4 || !all.Days[1].Last.LastTime.Equal(day.Add(15*time.Hour+10*time.Minute)) {
		t.Fatalf("day 0 last = %+v, want ledger 40's own rate and last_time", all.Days[1].Last)
	}
	if all.Days[1].Last.BSupply == nil || all.Days[1].Last.DSupply == nil {
		t.Fatalf("day 0 last carries no supplies: %+v", all.Days[1].Last)
	}

	// From day 1 the 15:10 state of day 0 closed before the slack: never read, but the probe sees it.
	late := read(day.AddDate(0, 0, 1), day.AddDate(0, 0, 4))
	want(late, [][3]uint32{{3, 60, 60}, {4, 65, 65}})
	if !late.Prior {
		t.Fatal("a state before the slack must set prior")
	}

	// The range bounds are half-open: day 2 alone holds only its own state.
	want(read(day.AddDate(0, 0, 2), day.AddDate(0, 0, 3)), [][3]uint32{{3, 60, 60}})

	if _, err := reader.LendingRateDays(ctx, c.pool, asset, day.Add(time.Hour), day.AddDate(0, 0, 1), cov); err == nil {
		t.Fatal("a range not on UTC midnights must be refused")
	}
	if _, err := reader.LendingRateDays(ctx, c.pool, asset, day, day, cov); err == nil {
		t.Fatal("an empty range must be refused")
	}
}

// rdL0 offsets the test's ledgers clear of other tests' stellar.ledgers rows.
const rdL0 = 88_400_000
