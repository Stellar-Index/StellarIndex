//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The rollup against real Postgres and ClickHouse: a first fill stores every covered day once; a re-derive of one day
// re-values exactly that day; a new source exclusion re-values the days it overlaps; a row of an older valuation
// revision is re-valued; a re-run writes nothing; and while one process holds the lock another writes nothing.
func TestLendingRevenueRollup_FillChangesAndLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	for _, s := range []string{`TRUNCATE TABLE stellar.lending_interest_hourly`, `TRUNCATE TABLE stellar.entry_history_watermark`} {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	d0 := time.Date(2031, 6, 1, 0, 0, 0, 0, time.UTC)
	day := func(i int) time.Time { return d0.Add(time.Duration(i) * 24 * time.Hour) }
	// Far below every other test's range: stellar.ledgers is shared, and its max is the lake tip other tests pin.
	const floor, thru = 2_718_001, 2_718_100
	for seq, at := range map[uint32]time.Time{floor: d0.Add(-time.Hour), thru: day(9).Add(24*time.Hour - time.Second)} {
		if err := conn.Exec(ctx, `INSERT INTO stellar.ledgers (ledger_seq, close_time, ledger_hash, prev_hash, protocol_version)
			VALUES (?, ?, ?, '00', 23)`, seq, at, fmt.Sprintf("%064d", seq)); err != nil {
			t.Fatal(err)
		}
	}
	const pool, asset = "CPOOLROLLUPA", "CASSETROLLUPX"
	for name, v := range map[string]uint32{"lending_interest": thru, "lending_interest_floor": floor, "lending_interest:" + pool: thru} {
		if err := conn.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES (?, ?)`, name, v); err != nil {
			t.Fatal(err)
		}
	}
	row := func(d int, gross int64) lendinginterest.HourRow {
		z := func() *big.Int { return new(big.Int) }
		g := big.NewInt(gross)
		return lendinginterest.HourRow{
			Pool: pool, Asset: asset, Protocol: lendinginterest.BlendV2, Hour: day(d).Add(5 * time.Hour), FirstLedger: 1, LastLedger: 2,
			Versions: 1, AccruedVersions: 1, Gross: g, ProtocolFee: z(), Backstop: z(), SupplierIntended: g, SupplierRealised: g,
			BackstopCreditDelta: z(), BackstopResidual: z(), SupplyResidual: z(), DefaultLoss: z(), FeeCreditResidual: z(),
			ResidualExplained: z(), ResidualAttributed: z(), InterestDrawn: z(), Gulped: z(), ReconcileBackstopLoss: z(),
		}
	}
	var rows []lendinginterest.HourRow
	for d := range 10 {
		rows = append(rows, row(d, int64(1000+d)))
	}
	if err := chstore.InsertLendingInterestRows(ctx, addr, rows, lendinginterest.DeriveRev); err != nil {
		t.Fatal(err)
	}
	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, db := openOrientationStore(t, ctx, dsn)
	srv := v1.New(v1.Options{Explorer: er})
	w := srv.NewLendingRevenueRollup(store, slog.Default())
	cycle := func(w *v1.LendingRevenueRollup) v1.LendingRevenueRollupStats {
		t.Helper()
		st, err := w.Cycle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	stored := func() map[time.Time]timescale.LendingRevenueUSDDay {
		t.Helper()
		got, err := store.LendingRevenueUSDDays(ctx, pool, day(0), day(9))
		if err != nil {
			t.Fatal(err)
		}
		out := map[time.Time]timescale.LendingRevenueUSDDay{}
		for _, d := range got {
			out[d.Day] = d
		}
		return out
	}

	// First fill: every covered day, once. No decimals are known, so each is stored unpriced with the walk's reason.
	if st := cycle(w); !st.Held || st.Written != 10 {
		t.Fatalf("first fill: %+v; want 10 rows written", st)
	}
	first := stored()
	if len(first) != 10 || first[day(3)].WalkReason == nil || *first[day(3)].WalkReason != "decimals_unknown" || first[day(3)].UnpricedHours != 1 {
		t.Fatalf("first fill rows: %d, day 3 %+v", len(first), first[day(3)])
	}
	var cov timescale.LendingRevenueUSDCoverage
	covs, err := store.LendingRevenueUSDCoverages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov = covs[pool]; !cov.FloorDay.Equal(day(0)) || !cov.ThroughDay.Equal(day(9)) {
		t.Fatalf("coverage %+v; want days 0 through 9", cov)
	}

	// Idempotent: a second cycle writes nothing.
	if st := cycle(w); st.Written != 0 {
		t.Fatalf("re-run wrote %d rows", st.Written)
	}

	// A re-derive of day 4 re-values exactly that day.
	time.Sleep(5 * time.Millisecond)
	if err := chstore.InsertLendingInterestRows(ctx, addr, []lendinginterest.HourRow{row(4, 9999)}, lendinginterest.DeriveRev); err != nil {
		t.Fatal(err)
	}
	if st := cycle(w); st.Written != 1 {
		t.Fatalf("re-derive cycle wrote %d rows; want exactly day 4", st.Written)
	}
	after := stored()
	for d, r := range after {
		changed := !r.InterestDerivedAt.Equal(first[d].InterestDerivedAt)
		if changed != d.Equal(day(4)) {
			t.Errorf("day %s: derived_at changed=%v", d.Format(time.DateOnly), changed)
		}
	}

	// A source exclusion created now re-values exactly the days it overlaps, beside the sweep's day and the trailing
	// days. Every stored row is first marked with a wrong hour count that only a re-valuation rewrites, so the rows
	// rewritten are the days re-valued: the sweep has reached day 2 (cycles 2-3 swept days 0-1), the exclusion covers
	// days 4-6, and days 7-9 are the trailing re-walk.
	if _, err := db.ExecContext(ctx, `INSERT INTO source_exclusions (source, from_ts, until_ts, reason, actor)
		VALUES ('soroswap', $1, $2, 'test', 'test')`, day(4), day(7)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE lending_revenue_usd_daily SET unpriced_hours = 2 WHERE pool = $1`, pool); err != nil {
		t.Fatal(err)
	}
	if st := cycle(w); st.Written != 7 || st.Mismatches != 1 {
		t.Fatalf("exclusion cycle %+v; want days 2 and 4-9 rewritten, the sweep's day 2 a mismatch", st)
	}
	for d, r := range stored() {
		revalued := r.UnpricedHours == 1
		if want := d.Equal(day(2)) || !d.Before(day(4)); revalued != want {
			t.Errorf("exclusion cycle: day %s re-valued=%v, want %v", d.Format(time.DateOnly), revalued, want)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE lending_revenue_usd_daily SET unpriced_hours = 1 WHERE pool = $1`, pool); err != nil {
		t.Fatal(err)
	}

	// A row of an older valuation revision is re-valued.
	if _, err := db.ExecContext(ctx, `UPDATE lending_revenue_usd_daily SET valuation_rev = 0 WHERE day = $1`, day(2)); err != nil {
		t.Fatal(err)
	}
	if st := cycle(w); st.Written != 1 {
		t.Fatalf("valuation_rev cycle wrote %d rows; want day 2 only", st.Written)
	}
	if stored()[day(2)].ValuationRev == 0 {
		t.Fatal("day 2 still at the old valuation revision")
	}

	// Two writers: while another process holds the lock, a cycle writes nothing.
	held, err := store.HoldInstanceLock(ctx, timescale.LendingRevenueRollupLockName, func() {}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE lending_revenue_usd_daily SET valuation_rev = 0`); err != nil {
		t.Fatal(err)
	}
	other := srv.NewLendingRevenueRollup(store, slog.Default())
	if st := cycle(other); st.Held || st.Written != 0 {
		t.Fatalf("second writer: %+v; want it to skip while the lock is held", st)
	}
	if err := held.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if st := cycle(other); !st.Held || st.Written != 10 {
		t.Fatalf("after release: %+v; want the lock taken and every day re-valued", st)
	}
}
