//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The revenue read sums only the newest derive of each hour (FINAL), only the current derive_rev (older rows counted
// as stale), only hours through the lower of the shared watermark and the pool's own coverage row, and only hours
// before a pending redo's first suspect one; a pool with no coverage row reads nothing.
func TestLendingPoolRevenue_FinalRevisionAndClipping(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	for _, s := range []string{`TRUNCATE TABLE stellar.lending_interest_hourly`, `TRUNCATE TABLE stellar.entry_history_watermark`} {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	h0 := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	hr := func(i int) time.Time { return h0.Add(time.Duration(i) * time.Hour) }
	const floor, poolBThru, redoFrom, wm = 900_000_001, 900_000_050, 900_000_070, 900_000_100
	for seq, at := range map[uint32]time.Time{
		floor: hr(0).Add(5 * time.Second), poolBThru: hr(1).Add(3599 * time.Second),
		redoFrom: hr(2).Add(10 * time.Second), wm: hr(3).Add(3599 * time.Second),
	} {
		if err := conn.Exec(ctx, `INSERT INTO stellar.ledgers (ledger_seq, close_time, ledger_hash, prev_hash, protocol_version)
			VALUES (?, ?, ?, '00', 23)`, seq, at, fmt.Sprintf("%064d", seq)); err != nil {
			t.Fatal(err)
		}
	}
	const poolA, poolB, poolC, assetX, assetY, assetZ = "CPOOLREVA", "CPOOLREVB", "CPOOLREVC", "CASSETREVX", "CASSETREVY", "CASSETREVZ"
	mark := func(name string, v uint32) {
		t.Helper()
		if err := conn.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES (?, ?)`, name, v); err != nil {
			t.Fatal(err)
		}
	}
	mark("lending_interest", wm)
	mark("lending_interest_floor", floor)
	mark("lending_interest:"+poolA, wm)
	mark("lending_interest:"+poolB, poolBThru)

	wide := new(big.Int).Lsh(big.NewInt(1), 130) // past Int128: the sum needs Int256
	row := func(pool, asset string, h int, gross *big.Int) lendinginterest.HourRow {
		z := func() *big.Int { return new(big.Int) }
		backstop := new(big.Int).Quo(gross, big.NewInt(5))
		return lendinginterest.HourRow{
			Pool: pool, Asset: asset, Protocol: lendinginterest.BlendV2, Hour: hr(h), FirstLedger: 1, LastLedger: 2, Versions: 1,
			AccruedVersions: 1, Gross: gross, ProtocolFee: z(), Backstop: backstop, SupplierIntended: new(big.Int).Sub(gross, backstop),
			SupplierRealised: new(big.Int).Sub(gross, backstop), BackstopCreditDelta: backstop, BackstopResidual: z(), SupplyResidual: z(),
			DefaultLoss: z(), FeeCreditResidual: z(), ResidualExplained: z(), ResidualAttributed: z(), InterestDrawn: z(), Gulped: z(),
			ReconcileBackstopLoss: z(),
		}
	}
	insert := func(rev uint16, rows ...lendinginterest.HourRow) {
		t.Helper()
		if err := chstore.InsertLendingInterestRows(ctx, addr, rows, rev); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond) // derived_at is ms: a later derive must be strictly newer
	}
	lossy := row(poolA, assetY, 0, big.NewInt(50))
	lossy.SupplyResidual, lossy.BRateMismatches, lossy.UnreconciledVersions = big.NewInt(-7), 1, 1
	cur := lendinginterest.DeriveRev
	insert(cur, row(poolA, assetX, 0, big.NewInt(100)), row(poolA, assetX, 1, big.NewInt(1)), lossy,
		row(poolA, assetX, 3, wide), row(poolA, assetX, 4, big.NewInt(1_000_000)), // hour 4 is past the watermark
		row(poolB, assetX, 1, big.NewInt(2)), row(poolB, assetX, 2, big.NewInt(3))) // poolB is covered through hour 1
	insert(cur, row(poolA, assetX, 1, big.NewInt(10))) // the newer derive of hour 1
	insert(cur-1, row(poolA, assetX, 2, big.NewInt(7)))
	// Asset Z's hour 0: a rev-4 row, then a newer rev-3 derive of the same hour. FINAL keeps the rev-3 row, which is
	// counted stale; the shadowed rev-4 row must not resurface.
	insert(cur, row(poolA, assetZ, 0, big.NewInt(9)))
	insert(cur-1, row(poolA, assetZ, 0, big.NewInt(8)))

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	q := chstore.LendingHistoryQuery{From: hr(0), To: hr(6), MaxPoints: 2000}
	read := func(pool, asset string, q chstore.LendingHistoryQuery) chstore.LendingRevenue {
		t.Helper()
		r, err := er.LendingPoolRevenue(ctx, pool, asset, q)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	grossOf := func(r chstore.LendingRevenueReserve) []string {
		var out []string
		for _, b := range r.Buckets {
			out = append(out, b.Start.Format("15")+"="+b.Sums.Gross.String())
		}
		return out
	}

	a := read(poolA, "", q)
	if !a.Covered || a.ThroughLedger != wm || !a.ThroughHour.Equal(hr(3)) || !a.Before.Equal(hr(4)) || a.FloorLedger != floor || len(a.Reserves) != 3 {
		t.Fatalf("pool A coverage = %+v", a)
	}
	x, y := a.Reserves[0], a.Reserves[1]
	if got := fmt.Sprint(grossOf(x)); x.Asset != assetX || got != "[00=100 01=10 03="+wide.String()+"]" || x.StaleRows != 1 {
		t.Fatalf("pool A X = %s stale %d", got, x.StaleRows)
	}
	if s := y.Buckets[0].Sums; y.Asset != assetY || s.UnexplainedSupplyLoss.Int64() != -7 || s.UnexplainedSupplyLossVersions != 1 || s.UnexplainedVersions != 1 ||
		s.UnexplainedSupplyGain.Sign() != 0 || s.UnexplainedSupplyGainVersions != 0 {
		t.Fatalf("pool A Y = %+v", s)
	}
	if z := a.Reserves[2]; z.Asset != assetZ || z.StaleRows != 1 || len(z.Buckets) != 0 {
		t.Fatalf("pool A Z: a newer rev-3 derive must shadow the rev-4 row, counted stale: %+v", z)
	}
	daily := q
	daily.Daily = true
	if d := read(poolA, assetX, daily); len(d.Reserves) != 1 || len(d.Reserves[0].Buckets) != 1 ||
		d.Reserves[0].Buckets[0].Sums.Gross.Cmp(new(big.Int).Add(wide, big.NewInt(110))) != 0 {
		t.Fatalf("pool A X daily = %+v", d.Reserves)
	}
	if b := read(poolB, "", q); !b.Before.Equal(hr(2)) || len(b.Reserves) != 1 || fmt.Sprint(grossOf(b.Reserves[0])) != "[01=2]" {
		t.Fatalf("pool B is clipped to its own coverage: %+v", b)
	}
	if c := read(poolC, "", q); c.Covered || c.Reserves != nil {
		t.Fatalf("pool C has no coverage row: %+v", c)
	}
	capped := q
	capped.MaxPoints = 2
	if _, err := er.LendingPoolRevenue(ctx, poolA, assetX, capped); err == nil {
		t.Fatal("a reserve past the point cap read without error")
	}

	// A pending redo from inside hour 2 leaves hours 2 on suspect.
	mark("lending_interest_redo_from:1", redoFrom)
	mark("lending_interest_redo", 1)
	r := read(poolA, assetX, q)
	if r.Redo == nil || !r.RedoHour.Equal(hr(2)) || !r.Before.Equal(hr(2)) || fmt.Sprint(grossOf(r.Reserves[0])) != "[00=100 01=10]" || r.Reserves[0].StaleRows != 0 {
		t.Fatalf("under a pending redo: %+v", r)
	}
}
