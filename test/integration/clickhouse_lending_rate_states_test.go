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

// TestLendingRateStates_BracketsDuplicatesBoundaryAndNoBase reads each reserve's ResData around a window: FINAL keeps an
// hour's last change and an older version never resurfaces through a bound, a state closing exactly at a bound is in force
// there, a base older than an hour before the window is never read, a reserve first written inside the window has a first
// state but no base, and the watermark's hour is withheld.
func TestLendingRateStates_BracketsDuplicatesBoundaryAndNoBase(t *testing.T) {
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

	var poolID, oldID, newID, noneID xdr.ContractId
	poolID[0], oldID[0], newID[0], noneID[0] = 0x81, 0x82, 0x83, 0x84
	enc := func(id xdr.ContractId) string { return strkey.MustEncode(strkey.VersionByteContract, id[:]) }
	oldAsset, newAsset, noneAsset := enc(oldID), enc(newID), enc(noneID)
	day := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	c := lendingHistoryCase{t: t, ctx: ctx, conn: conn, reader: reader, pool: enc(poolID), day: day}
	assets := []string{oldAsset, newAsset, noneAsset}

	if _, err := reader.LendingHistoryCoverage(ctx); !errors.Is(err, chstore.ErrLendingHistoryNotReady) {
		t.Fatalf("before any derive: err = %v, want not ready", err)
	}
	const base = 1_000_000_000_000
	put := func(asset xdr.ContractId, ledger uint32, at time.Duration, bRate uint64) {
		c.insert(resDataKeyB64(t, poolID, asset), ledger, at, resDataRatesEntryB64(t, poolID, asset, ledger, bRate, 2*bRate, uint64(day.Add(at).Unix())))
	}
	c.setWatermark(rsL0+1000, day.Add(40*time.Hour+30*time.Minute))
	put(oldID, rsL0+100, 8*time.Hour+30*time.Minute, base+1)
	put(oldID, rsL0+150, 9*time.Hour+40*time.Minute, base+15)
	put(oldID, rsL0+200, 10*time.Hour+20*time.Minute, base+2)
	put(oldID, rsL0+190, 10*time.Hour+5*time.Minute, base+99) // the hour's lower version, inserted last
	put(oldID, rsL0+300, 20*time.Hour, base+3)
	put(oldID, rsL0+400, 30*time.Hour+15*time.Minute, base+4)
	put(oldID, rsL0+950, 40*time.Hour+5*time.Minute, base+5) // the watermark's own hour
	put(newID, rsL0+500, 25*time.Hour, base+7)

	// At 10:10 hour 10 holds only its 10:20 change: the base is 09:40's, never ledger 190 resurfacing through the bound.
	w := c.rateStates(assets, day.Add(10*time.Hour+10*time.Minute), day.Add(48*time.Hour))
	c.wantStates(w, oldAsset, 150, 200, 400)
	c.wantStates(w, newAsset, 0, 500, 500)
	if _, ok := w.Reserves[noneAsset]; ok {
		t.Fatalf("a reserve with no row must be absent: %+v", w.Reserves[noneAsset])
	}
	if !w.Reserves[oldAsset].Prior || w.Reserves[newAsset].Prior {
		t.Fatalf("prior: old %v (08:30 is before the slack), new %v (nothing before)", w.Reserves[oldAsset].Prior, w.Reserves[newAsset].Prior)
	}
	// From 21:30 the 20:00 state closed before the slack: no base, but the probe sees a state there.
	if late := c.rateStates(assets, day.Add(21*time.Hour+30*time.Minute), day.Add(48*time.Hour)).Reserves[oldAsset]; late.Base != nil || !late.Prior || late.First == nil {
		t.Fatalf("21:30: %+v, want no base, a prior state and a first state", late)
	}
	st := w.Reserves[oldAsset]
	if !st.End.LastTime.Equal(day.Add(30*time.Hour+15*time.Minute)) || st.First.BRate.Uint64() != base+2 || st.Base.DRate.Uint64() != 2*(base+15) {
		t.Fatalf("states %+v / %+v / %+v: want the rows' own last_time and rates", st.Base, st.First, st.End)
	}

	// The base is the last state within the hour before From: 10:20's from 10:40, 08:30's from 09:25.
	c.wantStates(c.rateStates(assets, day.Add(10*time.Hour+40*time.Minute), day.Add(48*time.Hour)), oldAsset, 200, 300, 400)
	c.wantStates(c.rateStates(assets, day.Add(9*time.Hour+25*time.Minute), day.Add(48*time.Hour)), oldAsset, 100, 150, 400)

	// A state closing exactly at a bound is in force there, at either end.
	c.wantStates(c.rateStates(assets, day.Add(20*time.Hour), day.Add(30*time.Hour+15*time.Minute)), oldAsset, 300, 300, 400)

	// Before every state: absent.
	if w := c.rateStates(assets, day, day.Add(time.Hour)); len(w.Reserves) != 0 {
		t.Fatalf("window before every state: %+v", w.Reserves)
	}
	cov, _ := reader.LendingHistoryCoverage(ctx)
	if _, err := reader.LendingRateStates(ctx, c.pool, assets, day, day, time.Hour, cov); err == nil {
		t.Fatal("an empty window must be refused")
	}
}

// rsL0 offsets the test's ledgers clear of other tests' stellar.ledgers rows.
const rsL0 = 88_300_000

func (c lendingHistoryCase) rateStates(assets []string, from, to time.Time) chstore.LendingRateWindow {
	c.t.Helper()
	cov, err := c.reader.LendingHistoryCoverage(c.ctx)
	if err != nil || cov.ThroughLedger != rsL0+1000 {
		c.t.Fatalf("coverage %+v: %v", cov, err)
	}
	w, err := c.reader.LendingRateStates(c.ctx, c.pool, assets, from, to, time.Hour, cov)
	if err != nil {
		c.t.Fatalf("LendingRateStates: %v", err)
	}
	return w
}

// wantStates requires asset's base, first and end ledgers (offset by rsL0); 0 means no state.
func (c lendingHistoryCase) wantStates(w chstore.LendingRateWindow, asset string, baseL, firstL, endL uint32) {
	c.t.Helper()
	st, ok := w.Reserves[asset]
	if !ok || st.Undecodable {
		c.t.Fatalf("%s: states %+v present=%v", asset, st, ok)
	}
	check := func(side string, s *chstore.LendingRateState, want uint32) {
		switch {
		case want == 0 && s != nil:
			c.t.Fatalf("%s %s: got ledger %d, want none", asset, side, s.Ledger-rsL0)
		case want == 0:
		case s == nil:
			c.t.Fatalf("%s %s: none, want ledger %d", asset, side, want)
		case s.Ledger != rsL0+want:
			c.t.Fatalf("%s %s: ledger %d, want %d", asset, side, s.Ledger-rsL0, want)
		}
	}
	check("base", st.Base, baseL)
	check("first", st.First, firstL)
	check("end", st.End, endL)
}
