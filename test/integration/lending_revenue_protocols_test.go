//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The protocols route's one GROUP BY over lending_revenue_usd_daily, on real Postgres: legs come back as exact NUMERIC
// sums (no float on the way), days fall into the buckets counted back from the window end, rows of another revision are
// grouped apart, refused rows are counted and their hours kept out of the unpriced count, and the plan reads the primary key.
func TestLendingRevenueUSDBucketSums(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, db := openOrientationStore(t, ctx, dsn)

	end := time.Date(2031, 3, 31, 0, 0, 0, 0, time.UTC)
	back := func(n int) time.Time { return end.AddDate(0, 0, -n) }
	legs := func(v string) [7]*string {
		var l [7]*string
		for i := range l {
			l[i] = &v
		}
		return l
	}
	withheld := "withheld"
	row := func(pool, asset string, day time.Time, v string) timescale.LendingRevenueUSDDay {
		return timescale.LendingRevenueUSDDay{
			Pool: pool, Asset: asset, Day: day, InterestDeriveRev: 3, ValuationRev: 1,
			InterestDerivedAt: day, Legs: legs(v), HoursValued: 2, BasisHours: map[string]int{},
		}
	}
	const p1, p2, a = "CPOOLPROTOA", "CPOOLPROTOB", "CASSETPROTOX"
	tiny := "0.0000000000000000000000000001"
	rows := []timescale.LendingRevenueUSDDay{
		row(p1, a, back(0), "123456789012345678901234567890.1"), row(p1, a, back(0).AddDate(0, 0, 1), "999"), // past through
		row(p1, a, back(3), tiny), row(p1, a, back(6), "1"), // bucket 1
		row(p1, a, back(29), "2"), row(p1, a, back(30), "4"), row(p1, a, back(31), "8"), // bucket 2 ends at 29 days back
		row(p2, a, back(0), "5"),
	}
	stale := row(p1, a, back(1), "1000")
	stale.ValuationRev = 0
	refused := row(p1, a, back(2), "")
	refused.Legs, refused.HoursValued, refused.HoursUnvalued, refused.WalkReason = [7]*string{}, 0, 7, &withheld
	unpriced := row(p2, a, back(1), "6")
	unpriced.HoursUnvalued, unpriced.StaleRows, unpriced.AccrualExcluded = 3, 2, 1
	if err := store.UpsertLendingRevenueUSDDays(ctx, append(rows, stale, refused, unpriced)); err != nil {
		t.Fatal(err)
	}

	spans := []timescale.LendingRevenueUSDSpan{
		{Pool: p1, FromDay: back(100), ThroughDay: end, WindowEnd: end},
		{Pool: p2, FromDay: back(100), ThroughDay: end, WindowEnd: end},
	}
	got, err := store.LendingRevenueUSDBucketSums(ctx, spans, 1, 3, withheld)
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		pool    string
		bucket  int
		current bool
	}
	by := map[key]timescale.LendingRevenueUSDBucketSum{}
	for _, g := range got {
		by[key{g.Pool, g.Bucket, g.Current}] = g
	}
	leg := func(k key) string {
		if l := by[k].Legs[0]; l != nil {
			return *l
		}
		return "null"
	}
	for k, want := range map[key]string{
		{p1, 0, true}: "123456789012345678901234567890.1", {p1, 1, true}: "1.0000000000000000000000000001",
		{p1, 2, true}: "2", {p1, 3, true}: "12", {p1, 1, false}: "1000", {p2, 0, true}: "5", {p2, 1, true}: "6",
	} {
		if leg(k) != want {
			t.Errorf("%+v interest_accrued_usd = %s; want %s exactly", k, leg(k), want)
		}
	}
	if g := by[key{p1, 1, true}]; g.Days != 3 || g.WithheldDays != 1 || g.HoursUnvalued != 0 || !g.LastAmountDay.Equal(back(2)) {
		t.Errorf("p1 bucket 1: days %d withheld %d unpriced hours %d last amount day %s; want 3, 1, 0, %s",
			g.Days, g.WithheldDays, g.HoursUnvalued, g.LastAmountDay, back(2))
	}
	if g := by[key{p2, 1, true}]; g.HoursUnvalued != 3 || g.ExcludedCount != 3 {
		t.Errorf("p2 bucket 1: unpriced hours %d excluded %d; want 3, 3", g.HoursUnvalued, g.ExcludedCount)
	}
	if len(got) != 7 {
		t.Errorf("%d groups; want 7: %+v", len(got), got)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	day := func(d time.Time) string { return d.Format(time.DateOnly) }
	plan, err := conn.QueryContext(ctx, `EXPLAIN `+timescale.LendingRevenueUSDBucketSumsQuery,
		[]string{p1}, []string{day(back(100))}, []string{day(end)}, []string{day(end)}, 1, 3, withheld)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plan.Close() }()
	var lines []string
	for plan.Next() {
		var l string
		if err := plan.Scan(&l); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
	}
	if text := strings.Join(lines, "\n"); !strings.Contains(text, "lending_revenue_usd_daily_pkey") {
		t.Errorf("plan does not read the primary key:\n%s", text)
	}
}
