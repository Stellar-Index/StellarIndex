package v1

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

var rollupD0 = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func rollupDay(i int) time.Time { return rollupD0.Add(time.Duration(i) * day24) }

func rollupAsset(pool string) string { return "A" + pool }

// fakeRollupLake holds each pool's interest days from rollupD0, one reserve each, one hour carrying an amount per day.
type fakeRollupLake struct {
	days    map[string]int
	derived map[reserveDay]time.Time // newest derived_at; rollupD0 when absent
	clip    map[string]bool          // pools whose run reads come back uncovered
	// dayList rewrites the interest days' Day as a broken reader might (nil: as read); failRun fails the run reads.
	dayList func(d time.Time) (time.Time, bool)
	failRun bool
}

func (l *fakeRollupLake) derivedAt(pool string, d time.Time) time.Time {
	if at, ok := l.derived[reserveDay{pool, rollupAsset(pool), d}]; ok {
		return at
	}
	return rollupD0
}

func (l *fakeRollupLake) LendingPoolRevenue(_ context.Context, pool, _ string, q clickhouse.LendingHistoryQuery) (clickhouse.LendingRevenue, error) {
	if q.Daily && l.failRun {
		return clickhouse.LendingRevenue{}, errors.New("run read failed")
	}
	end := rollupDay(l.days[pool])
	rev := clickhouse.LendingRevenue{
		Covered: !(q.Daily && l.clip[pool]), FloorLedger: 1, FloorTime: rollupD0.Add(-time.Hour), ThroughHour: end.Add(-time.Hour),
		From: q.From, Before: minTime(q.To, end),
	}
	res := clickhouse.LendingRevenueReserve{Asset: rollupAsset(pool)}
	for d := maxTime(q.From.Truncate(day24), rollupD0); d.Before(rev.Before); d = d.Add(day24) {
		if h := d.Add(5 * time.Hour); !h.Before(q.From) && h.Before(rev.Before) {
			s := clickhouse.NewLendingRevenueSums()
			s.Gross.SetInt64(1000)
			res.Hours = append(res.Hours, clickhouse.LendingRevenueBucket{Start: h, Sums: s})
		}
	}
	rev.Reserves = []clickhouse.LendingRevenueReserve{res}
	return rev, nil
}

func (l *fakeRollupLake) LendingInterestPoolDays(_ context.Context, pool string, from, before time.Time) ([]clickhouse.LendingInterestDay, error) {
	var out []clickhouse.LendingInterestDay
	for d := maxTime(from, rollupD0); d.Before(before) && d.Before(rollupDay(l.days[pool])); d = d.Add(day24) {
		day, ok := d, true
		if l.dayList != nil {
			day, ok = l.dayList(d)
		}
		if ok {
			out = append(out, clickhouse.LendingInterestDay{Pool: pool, Asset: rollupAsset(pool), Day: day, DerivedAt: l.derivedAt(pool, d)})
		}
	}
	return out, nil
}

func changeLess(a, b clickhouse.LendingInterestDay) bool {
	switch {
	case !a.DerivedAt.Equal(b.DerivedAt):
		return a.DerivedAt.Before(b.DerivedAt)
	case a.Pool != b.Pool:
		return a.Pool < b.Pool
	case a.Asset != b.Asset:
		return a.Asset < b.Asset
	}
	return a.Day.Before(b.Day)
}

func (l *fakeRollupLake) LendingInterestChangedDays(_ context.Context, after clickhouse.LendingInterestDay, limit int) ([]clickhouse.LendingInterestDay, error) {
	var out []clickhouse.LendingInterestDay
	for pool, n := range l.days {
		for i := range n {
			c := clickhouse.LendingInterestDay{Pool: pool, Asset: rollupAsset(pool), Day: rollupDay(i), DerivedAt: l.derivedAt(pool, rollupDay(i))}
			if changeLess(after, c) {
				out = append(out, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return changeLess(out[i], out[j]) })
	return out[:min(limit, len(out))], nil
}

func (l *fakeRollupLake) LendingInterestPools(context.Context) ([]string, error) {
	var out []string
	for pool := range l.days {
		out = append(out, pool)
	}
	sort.Strings(out)
	return out, nil
}

type fakeExclusion struct {
	created time.Time
	window  [2]time.Time
}

type fakeRollupStore struct {
	rows       map[reserveDay]timescale.LendingRevenueUSDDay
	covs       map[string]timescale.LendingRevenueUSDCoverage
	exclusions []fakeExclusion
	failStale  int
}

func (s *fakeRollupStore) HoldInstanceLock(context.Context, string, func(), *slog.Logger) (*timescale.InstanceLock, error) {
	return nil, nil
}

func (s *fakeRollupStore) LendingRevenueUSDDays(_ context.Context, pool string, from, through time.Time) ([]timescale.LendingRevenueUSDDay, error) {
	var out []timescale.LendingRevenueUSDDay
	for k, d := range s.rows {
		if k.pool == pool && !k.day.Before(from) && !k.day.After(through) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *fakeRollupStore) UpsertLendingRevenueUSDDays(_ context.Context, rows []timescale.LendingRevenueUSDDay) error {
	for _, d := range rows {
		s.rows[reserveDay{d.Pool, d.Asset, d.Day}] = d
	}
	return nil
}

func (s *fakeRollupStore) LendingRevenueUSDStaleRevDays(context.Context, int16, int16, int) (map[string][]time.Time, error) {
	if s.failStale > 0 {
		s.failStale--
		return nil, errors.New("stale read failed")
	}
	return nil, nil
}

func (s *fakeRollupStore) LendingRevenueUSDNewestDerivedAt(context.Context) (time.Time, error) {
	var at time.Time
	for _, d := range s.rows {
		at = maxTime(at, d.InterestDerivedAt)
	}
	return at, nil
}

func (s *fakeRollupStore) LendingRevenueUSDCoverages(context.Context) (map[string]timescale.LendingRevenueUSDCoverage, error) {
	out := map[string]timescale.LendingRevenueUSDCoverage{}
	for k, c := range s.covs {
		out[k] = c
	}
	return out, nil
}

func (s *fakeRollupStore) UpsertLendingRevenueUSDCoverage(_ context.Context, c timescale.LendingRevenueUSDCoverage) error {
	s.covs[c.Pool] = c
	return nil
}

func (s *fakeRollupStore) SourceExclusionWindowsChangedSince(_ context.Context, since time.Time) ([][2]time.Time, error) {
	var out [][2]time.Time
	for _, e := range s.exclusions {
		if e.created.After(since) {
			out = append(out, e.window)
		}
	}
	return out, nil
}

// rollupHarness runs the rollup over the fakes on a clock that moves one cadence per cycle, logging every walk.
type rollupHarness struct {
	t       *testing.T
	lake    *fakeRollupLake
	store   *fakeRollupStore
	r       *LendingRevenueRollup
	clock   time.Time
	outcome map[reserveDay]reserveUSDRead // per (asset, day); unpriced when absent
	walked  map[reserveDay]bool
}

func newRollupHarness(t *testing.T, days map[string]int) *rollupHarness {
	h := &rollupHarness{
		t: t, lake: &fakeRollupLake{days: days, derived: map[reserveDay]time.Time{}, clip: map[string]bool{}},
		store: &fakeRollupStore{rows: map[reserveDay]timescale.LendingRevenueUSDDay{}, covs: map[string]timescale.LendingRevenueUSDCoverage{}},
		clock: rollupDay(1000), outcome: map[reserveDay]reserveUSDRead{}, walked: map[reserveDay]bool{},
	}
	h.r = &LendingRevenueRollup{
		store: h.store, lake: h.lake, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return h.clock },
		retry: map[poolDay]bool{},
		walk: func(_ context.Context, asset string, day time.Time) reserveUSDRead {
			k := reserveDay{asset: asset, day: day}
			h.walked[k] = true
			if o, ok := h.outcome[k]; ok {
				return o
			}
			return reserveUSDRead{series: reserveUSDSeries{reason: backstopUSDUnpriced}}
		},
	}
	return h
}

func (h *rollupHarness) cycle() (LendingRevenueRollupStats, error) {
	h.clock = h.clock.Add(lendingRevenueRollupCycle)
	return h.r.Cycle(context.Background())
}

func (h *rollupHarness) mustCycle() LendingRevenueRollupStats {
	h.t.Helper()
	st, err := h.cycle()
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func (h *rollupHarness) row(pool string, i int) (timescale.LendingRevenueUSDDay, bool) {
	d, ok := h.store.rows[reserveDay{pool, rollupAsset(pool), rollupDay(i)}]
	return d, ok
}

func (h *rollupHarness) through(pool string) time.Time { return h.store.covs[pool].ThroughDay }

var (
	rollupDegraded = reserveUSDRead{series: reserveUSDSeries{reason: exclusionPriceReadFailed, degraded: true}}
	rollupRefused  = reserveUSDRead{series: reserveUSDSeries{reason: exclusionPriceWithheld}, verdict: true}
)

// A fill day whose walk is degraded caps coverage the day before it, so a missing row inside coverage never hides it.
func TestLendingRevenueRollup_DegradedFillDayCapsCoverage(t *testing.T) {
	h := newRollupHarness(t, map[string]int{"P": 10})
	h.outcome[reserveDay{asset: "AP", day: rollupDay(4)}] = rollupDegraded
	if st := h.mustCycle(); st.Degraded != 1 || st.Written != 9 {
		t.Fatalf("first cycle %+v; want day 4 degraded, the other 9 written", st)
	}
	if _, ok := h.row("P", 4); ok {
		t.Fatal("a degraded day was stored")
	}
	if got := h.through("P"); !got.Equal(rollupDay(3)) {
		t.Fatalf("coverage through %s; want day 3, before the degraded fill day", got.Format(time.DateOnly))
	}
	delete(h.outcome, reserveDay{asset: "AP", day: rollupDay(4)})
	h.mustCycle()
	if _, ok := h.row("P", 4); !ok || !h.through("P").Equal(rollupDay(9)) {
		t.Fatalf("after recovery: day 4 stored=%v, through %s; want stored, day 9", ok, h.through("P").Format(time.DateOnly))
	}
}

// A verdict refusal is a final outcome: stored with its legs NULL, every amount hour unpriced, never a zero.
func TestLendingRevenueRollup_RefusedDayIsStoredUnpriced(t *testing.T) {
	h := newRollupHarness(t, map[string]int{"P": 10})
	h.outcome[reserveDay{asset: "AP", day: rollupDay(2)}] = rollupRefused
	if st := h.mustCycle(); st.Refused != 1 || st.Written != 10 {
		t.Fatalf("cycle %+v; want day 2 refused and all 10 days written", st)
	}
	row, ok := h.row("P", 2)
	if !ok || row.HoursValued != 0 || row.HoursUnvalued != 1 || row.WalkReason == nil || *row.WalkReason != exclusionPriceWithheld {
		t.Fatalf("refused row %+v (stored=%v); want unpriced with reason %q", row, ok, exclusionPriceWithheld)
	}
	for i, l := range row.Legs {
		if l != nil {
			t.Errorf("leg %d = %q; a refused day's legs are NULL", i, *l)
		}
	}
	if !h.through("P").Equal(rollupDay(9)) {
		t.Fatalf("coverage through %s; want day 9", h.through("P").Format(time.DateOnly))
	}
}

// A run the lake answers uncovered values nothing, and coverage stays where it was.
func TestLendingRevenueRollup_ClippedRunHoldsCoverage(t *testing.T) {
	h := newRollupHarness(t, map[string]int{"P": 10})
	h.lake.clip["P"] = true
	if st := h.mustCycle(); st.Written != 0 {
		t.Fatalf("clipped cycle wrote %d rows", st.Written)
	}
	if got := h.through("P"); !got.Equal(rollupDay(-1)) {
		t.Fatalf("coverage through %s; want the day before the floor", got.Format(time.DateOnly))
	}
	h.lake.clip["P"] = false
	if st := h.mustCycle(); st.Written != 10 || !h.through("P").Equal(rollupDay(9)) {
		t.Fatalf("unclipped cycle %+v, through %s; want 10 rows, day 9", st, h.through("P").Format(time.DateOnly))
	}
}

// Coverage moves over a queued day only once its valuation ran: a run read that fails, or interest days that do not
// account for the hours the read returned (a day list in the server's zone, or none at all), fail the cycle with
// coverage and the table unmoved, never a cycle that claims days it stored nothing for.
func TestLendingRevenueRollup_UnvaluedHoursHoldCoverage(t *testing.T) {
	plus2 := time.FixedZone("UTC+2", 2*3600)
	for name, set := range map[string]func(l *fakeRollupLake){
		"run read fails": func(l *fakeRollupLake) { l.failRun = true },
		"days at the server's midnight": func(l *fakeRollupLake) {
			l.dayList = func(d time.Time) (time.Time, bool) {
				return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, plus2).UTC(), true
			}
		},
		"no interest days": func(l *fakeRollupLake) { l.dayList = func(time.Time) (time.Time, bool) { return time.Time{}, false } },
		"one day missing": func(l *fakeRollupLake) {
			l.dayList = func(d time.Time) (time.Time, bool) { return d, !d.Equal(rollupDay(4)) }
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRollupHarness(t, map[string]int{"P": 10})
			set(h.lake)
			st, err := h.cycle()
			if err == nil {
				t.Fatalf("cycle %+v succeeded; want it failed", st)
			}
			if c, ok := h.store.covs["P"]; ok {
				t.Fatalf("coverage stored through %s over days no row was stored for", c.ThroughDay.Format(time.DateOnly))
			}
			h.lake.failRun, h.lake.dayList = false, nil
			if st := h.mustCycle(); st.Written != 10 || !h.through("P").Equal(rollupDay(9)) {
				t.Fatalf("recovered cycle %+v, through %s; want 10 rows, day 9", st, h.through("P").Format(time.DateOnly))
			}
		})
	}
}

// A pool-day the planner queues off a UTC midnight fails the cycle before any read.
func TestDayRollup_QueuedDayOffMidnightFails(t *testing.T) {
	d := &dayRollup{src: dayRollupSource{name: "test", budget: lendingRevenueRollupBudget}, retry: map[poolDay]bool{}}
	days := []poolDay{{"P", rollupDay(3).Add(-2 * time.Hour)}}
	if _, err := d.executePool(context.Background(), &rollupCycle{st: &LendingRevenueRollupStats{}}, days); err == nil {
		t.Fatal("a day off a UTC midnight was valued")
	}
}

// A degraded day queued by an exclusion is retried after a cycle whose planning read failed: the retry is dropped only
// once the day reaches a final outcome.
func TestLendingRevenueRollup_RetrySurvivesAFailedCycle(t *testing.T) {
	h := newRollupHarness(t, map[string]int{"P": 10})
	h.mustCycle()
	k := reserveDay{asset: "AP", day: rollupDay(5)}
	h.store.exclusions = append(h.store.exclusions, fakeExclusion{created: h.clock.Add(time.Second), window: [2]time.Time{rollupDay(5), rollupDay(6)}})
	h.outcome[k] = rollupDegraded
	if st := h.mustCycle(); st.Degraded != 1 || !h.walked[k] {
		t.Fatalf("exclusion cycle %+v; want day 5 walked and degraded", st)
	}
	h.store.failStale = 1
	if _, err := h.cycle(); err == nil {
		t.Fatal("the planning read failure did not fail the cycle")
	}
	delete(h.outcome, k)
	h.walked = map[reserveDay]bool{}
	h.mustCycle()
	if !h.walked[k] {
		t.Fatal("day 5 was not re-valued: its retry was lost")
	}
	h.walked = map[reserveDay]bool{}
	h.mustCycle()
	if h.walked[k] {
		t.Fatal("day 5 still retried after a final outcome")
	}
}

// An exclusion window wider than a cycle's budget resumes where the budget cut it: every stored day it overlaps is
// re-valued within two cycles.
func TestLendingRevenueRollup_ExclusionWindowResumesAfterBudgetCut(t *testing.T) {
	days := map[string]int{}
	for i := range 30 {
		days[fmt.Sprintf("P%02d", i)] = 20
	}
	h := newRollupHarness(t, days)
	for range 10 {
		h.mustCycle()
	}
	for pool := range days {
		if !h.through(pool).Equal(rollupDay(19)) {
			t.Fatalf("setup: %s through %s", pool, h.through(pool).Format(time.DateOnly))
		}
	}
	h.store.exclusions = append(h.store.exclusions, fakeExclusion{created: h.clock.Add(time.Second), window: [2]time.Time{rollupDay(0), rollupDay(20)}})
	h.walked = map[reserveDay]bool{}
	h.mustCycle()
	h.mustCycle()
	for pool := range days {
		for i := range 20 {
			if !h.walked[reserveDay{asset: rollupAsset(pool), day: rollupDay(i)}] {
				t.Errorf("%s day %d not re-valued after the exclusion", pool, i)
			}
		}
	}
}

// A re-derive of 2,000 pool-days sharing one derived_at, wider than any cycle's budget, is re-valued across cycles
// (days already current cost no budget), and another pool's self-fill advances every cycle meanwhile.
func TestLendingRevenueRollup_WideRederiveDoesNotStarveFill(t *testing.T) {
	days := map[string]int{"Z": 3000}
	for i := range 100 {
		days[fmt.Sprintf("P%03d", i)] = 20
	}
	h := newRollupHarness(t, days)
	for range 40 {
		h.mustCycle()
	}
	for i := range 100 {
		if pool := fmt.Sprintf("P%03d", i); !h.through(pool).Equal(rollupDay(19)) {
			t.Fatalf("setup: %s through %s", pool, h.through(pool).Format(time.DateOnly))
		}
	}
	w := h.clock.Add(time.Second)
	for i := range 100 {
		pool := fmt.Sprintf("P%03d", i)
		for d := range 20 {
			h.lake.derived[reserveDay{pool, rollupAsset(pool), rollupDay(d)}] = w
		}
	}
	current := func() int {
		n := 0
		for k, r := range h.store.rows {
			if k.pool != "Z" && r.InterestDerivedAt.Equal(w) {
				n++
			}
		}
		return n
	}
	for c := 0; current() < 2000; c++ {
		if c == 12 {
			t.Fatalf("after %d cycles only %d of 2000 re-derived days re-valued", c, current())
		}
		before := h.through("Z")
		h.mustCycle()
		if !h.through("Z").After(before) {
			t.Fatalf("cycle %d: pool Z's fill did not advance past %s", c, before.Format(time.DateOnly))
		}
	}
}
