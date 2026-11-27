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
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// rollupXLMPrice is XLM at exactly 1/8 USD in every hour, over the real store for everything else.
type rollupXLMPrice struct{ apiHistoryAdapter }

func (r rollupXLMPrice) HistoryPointsInRange(_ context.Context, pair c.Pair, _ string, from, to time.Time, _ int) ([]v1.HistoryPoint, error) {
	if pair.String() != "native/fiat:USD" {
		return nil, nil
	}
	var out []v1.HistoryPoint
	for h := from.Truncate(time.Hour); h.Before(to); h = h.Add(time.Hour) {
		out = append(out, v1.HistoryPoint{Bucket: h, VWAP: "0.125"})
	}
	return out, nil
}

// The rollup against real Postgres and ClickHouse: a first fill stores every covered day once, a priced reserve's legs
// exactly; a re-derive of one day re-values exactly that day; a new source exclusion re-values exactly the days it
// overlaps; a row of an older valuation revision is re-valued to the same exact legs; a re-run writes nothing; and while
// one process holds the lock another writes nothing.
func TestLendingRevenueRollup_FillChangesAndLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	// The rollup values every pool in the table, so it starts from an empty one, as its sibling lending tests do.
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
	xlm := c.NativeSACContractID()
	for name, v := range map[string]uint32{"lending_interest": thru, "lending_interest_floor": floor, "lending_interest:" + pool: thru} {
		if err := conn.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES (?, ?)`, name, v); err != nil {
			t.Fatal(err)
		}
	}
	z := func() *big.Int { return new(big.Int) }
	row := func(a string, d int, gross int64) lendinginterest.HourRow {
		g := big.NewInt(gross)
		return lendinginterest.HourRow{
			Pool: pool, Asset: a, Protocol: lendinginterest.BlendV2, Hour: day(d).Add(5 * time.Hour), FirstLedger: 1, LastLedger: 2,
			Versions: 1, AccruedVersions: 1, Gross: g, ProtocolFee: z(), Backstop: z(), SupplierIntended: g, SupplierRealised: g,
			BackstopCreditDelta: z(), BackstopResidual: z(), SupplyResidual: z(), DefaultLoss: z(), FeeCreditResidual: z(),
			ResidualExplained: z(), ResidualAttributed: z(), InterestDrawn: z(), Gulped: z(), ReconcileBackstopLoss: z(),
		}
	}
	// XLM's legs in base units: gross, supplier share, supplier credited, backstop take, protocol fee, backstop drawn,
	// default loss.
	xlmLegs := func(d int) [7]int64 {
		g := int64(1_000_000 + d)
		return [7]int64{g, g - 200_000, g - 200_001, 200_000, 7, 3, 2}
	}
	priced := func(d int) lendinginterest.HourRow {
		l := xlmLegs(d)
		r := row(xlm, d, l[0])
		r.SupplierIntended, r.SupplierRealised, r.Backstop = big.NewInt(l[1]), big.NewInt(l[2]), big.NewInt(l[3])
		r.ProtocolFee, r.InterestDrawn, r.DefaultLoss = big.NewInt(l[4]), big.NewInt(-l[5]), big.NewInt(-l[6])
		return r
	}
	var rows []lendinginterest.HourRow
	for d := range 10 {
		rows = append(rows, row(asset, d, int64(1000+d)), priced(d))
	}
	if err := chstore.InsertLendingInterestRows(ctx, addr, rows, lendinginterest.DeriveRev); err != nil {
		t.Fatal(err)
	}
	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}

	// Change detection pages by keyset through days sharing one derived_at, each exactly once.
	seen := map[string]bool{}
	for cur := (chstore.LendingInterestDay{}); ; {
		page, err := er.LendingInterestChangedDays(ctx, cur, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, d := range page {
			k := d.Asset + d.Day.Format(time.DateOnly)
			if seen[k] {
				t.Fatalf("change page repeated %s", k)
			}
			seen[k] = true
		}
		cur = page[len(page)-1]
	}
	if len(seen) != 20 {
		t.Fatalf("change pages held %d days; want all 20", len(seen))
	}

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, db := openOrientationStore(t, ctx, dsn)
	srv := v1.New(v1.Options{Explorer: er, History: rollupXLMPrice{apiHistoryAdapter{s: store}}})
	w := srv.NewLendingRevenueRollup(store, slog.Default())
	cycle := func(w *v1.LendingRevenueRollup) v1.LendingRevenueRollupStats {
		t.Helper()
		st, err := w.Cycle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	stored := func(a string) map[time.Time]timescale.LendingRevenueUSDDay {
		t.Helper()
		got, err := store.LendingRevenueUSDDays(ctx, pool, day(0), day(9))
		if err != nil {
			t.Fatal(err)
		}
		out := map[time.Time]timescale.LendingRevenueUSDDay{}
		for _, d := range got {
			if d.Asset == a {
				out[d.Day] = d
			}
		}
		return out
	}

	// First fill: every covered day of both reserves, once. Asset X's decimals are unknown, so it is stored unpriced with
	// the walk's reason; XLM is priced, each leg exactly its base units / 8 / 10^7.
	if st := cycle(w); !st.Held || st.Written != 20 {
		t.Fatalf("first fill: %+v; want 20 rows written", st)
	}
	first := stored(asset)
	if len(first) != 10 || first[day(3)].WalkReason == nil || *first[day(3)].WalkReason != "decimals_unknown" || first[day(3)].HoursUnvalued != 1 {
		t.Fatalf("first fill rows: %d, day 3 %+v", len(first), first[day(3)])
	}
	firstXLM := stored(xlm)
	for d := range 10 {
		r := firstXLM[day(d)]
		if r.HoursValued != 1 || r.HoursUnvalued != 0 || r.WalkReason != nil {
			t.Fatalf("XLM day %d: %+v; want one priced hour", d, r)
		}
		for i, units := range xlmLegs(d) {
			want := new(big.Rat).SetFrac(big.NewInt(units), big.NewInt(80_000_000))
			got, ok := new(big.Rat).SetString(legText(r.Legs[i], "x"))
			if !ok || got.Cmp(want) != 0 {
				t.Errorf("XLM day %d leg %d = %s; want exactly %s", d, i, legText(r.Legs[i], "NULL"), want.FloatString(12))
			}
		}
	}
	covs, err := store.LendingRevenueUSDCoverages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov := covs[pool]; !cov.FloorDay.Equal(day(0)) || !cov.ThroughDay.Equal(day(9)) {
		t.Fatalf("coverage %+v; want days 0 through 9", cov)
	}

	// Idempotent: a second cycle writes nothing.
	if st := cycle(w); st.Written != 0 {
		t.Fatalf("re-run wrote %d rows", st.Written)
	}

	// A re-derive of X's day 4 re-values exactly that row.
	time.Sleep(5 * time.Millisecond)
	if err := chstore.InsertLendingInterestRows(ctx, addr, []lendinginterest.HourRow{row(asset, 4, 9999)}, lendinginterest.DeriveRev); err != nil {
		t.Fatal(err)
	}
	if st := cycle(w); st.Written != 1 {
		t.Fatalf("re-derive cycle wrote %d rows; want exactly X's day 4", st.Written)
	}
	for d, r := range stored(asset) {
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
	if _, err := db.ExecContext(ctx, `UPDATE lending_revenue_usd_daily SET hours_unvalued = 2 WHERE pool = $1`, pool); err != nil {
		t.Fatal(err)
	}
	if st := cycle(w); st.Written != 14 || st.Mismatches != 2 {
		t.Fatalf("exclusion cycle %+v; want days 2 and 4-9 of both reserves rewritten, the sweep's day 2 a mismatch", st)
	}
	for _, a := range []string{asset, xlm} {
		for d, r := range stored(a) {
			if want := d.Equal(day(2)) || !d.Before(day(4)); (r.HoursUnvalued != 2) != want {
				t.Errorf("exclusion cycle: %s day %s re-valued=%v, want %v", a, d.Format(time.DateOnly), r.HoursUnvalued != 2, want)
			}
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE lending_revenue_usd_daily SET hours_unvalued = CASE WHEN asset = $2 THEN 1 ELSE 0 END
		WHERE pool = $1`, pool, asset); err != nil {
		t.Fatal(err)
	}

	// A row of an older valuation revision is re-valued, and an upsert replaces the legs exactly: never adds to them.
	if _, err := db.ExecContext(ctx, `UPDATE lending_revenue_usd_daily SET valuation_rev = 0 WHERE day = $1`, day(2)); err != nil {
		t.Fatal(err)
	}
	if st := cycle(w); st.Written != 2 {
		t.Fatalf("valuation_rev cycle wrote %d rows; want day 2 of both reserves", st.Written)
	}
	again := stored(xlm)[day(2)]
	if again.ValuationRev == 0 {
		t.Fatal("day 2 still at the old valuation revision")
	}
	for i := range again.Legs {
		if legText(again.Legs[i], "NULL") != legText(firstXLM[day(2)].Legs[i], "NULL") {
			t.Errorf("XLM day 2 leg %d re-valued to %s; was %s", i, legText(again.Legs[i], "NULL"), legText(firstXLM[day(2)].Legs[i], "NULL"))
		}
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
	if st := cycle(other); !st.Held || st.Written != 20 {
		t.Fatalf("after release: %+v; want the lock taken and every row re-valued", st)
	}
}

func legText(s *string, alt string) string {
	if s == nil {
		return alt
	}
	return *s
}
