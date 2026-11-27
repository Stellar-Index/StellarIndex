package v1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeDayRollup is a second instantiation of the day-rollup cycle: one pool whose day values are integers, changes a
// keyset list, and nothing of revenue's reads, valuer or tables. Its days go through the same decideRollupDay.
type fakeDayRollup struct {
	span        rollupSpan
	covs        map[string]rollupCoverage
	rows        map[poolDay]int64 // stored values
	truth       map[poolDay]int64 // what valuing a day gives now
	failing     map[poolDay]bool  // valued with no final outcome
	currentRows map[poolDay]bool  // stored rows already current for their source
	changes     []poolDay
	cursor      int // changes already got past
	failRuns    int // runs whose store fails, before any write
	held        bool
	booted      bool
	clock       time.Time
	spanReads   int
	since       []time.Time // exclusionsChangedSince calls
	valued      []poolDay   // this cycle's valuations, in order
	tasks       map[poolDay]rollupTask
	mismatched  int
	d           *dayRollup
}

const fakePool = "P"

type fakeChange struct {
	i  int
	pd poolDay
}

var fakeRollupRows = rollupRows[int64]{
	equal:       func(a, b int64) bool { return a == b },
	unexplained: func(a, b int64) bool { return a != b },
}

func newFakeDayRollup(days int) *fakeDayRollup {
	f := &fakeDayRollup{
		span: rollupSpan{floor: rollupDay(0), through: rollupDay(days - 1)}, covs: map[string]rollupCoverage{},
		rows: map[poolDay]int64{}, truth: map[poolDay]int64{}, failing: map[poolDay]bool{}, currentRows: map[poolDay]bool{},
		clock: rollupDay(days + 10), tasks: map[poolDay]rollupTask{},
	}
	for i := range days {
		f.truth[poolDay{fakePool, rollupDay(i)}] = int64(i) + 1
	}
	f.d = &dayRollup{
		src: dayRollupSource{
			name: "fake rollup", lockName: "fake", budget: lendingRevenueRollupBudget, ready: func() error { return nil },
			lock: func(context.Context, string, func(), *slog.Logger) (*timescale.InstanceLock, error) {
				if f.held {
					return nil, timescale.ErrInstanceLockHeld
				}
				return nil, nil
			},
			spans: func(context.Context) (map[string]rollupSpan, map[string]rollupCoverage, error) {
				f.spanReads++
				covs := map[string]rollupCoverage{}
				for k, c := range f.covs {
					covs[k] = c
				}
				return map[string]rollupSpan{fakePool: f.span}, covs, nil
			},
			bootstrap: func(context.Context, time.Time) (bool, error) {
				booted := !f.booted
				f.booted = true
				return booted, nil
			},
			changes: f.planChanges,
			stale:   func(context.Context, *rollupPlan) error { return nil },
			exclusionsChangedSince: func(_ context.Context, since time.Time) ([][2]time.Time, error) {
				f.since = append(f.since, since)
				return nil, nil
			},
			walk:     func(context.Context, string, time.Time) reserveUSDRead { return reserveUSDRead{} },
			valueRun: f.valueRun,
			storeCoverage: func(_ context.Context, pool string, sp rollupSpan, through time.Time) error {
				f.covs[pool] = rollupCoverage{floorDay: sp.floor, throughDay: through, current: true}
				return nil
			},
			started: func(time.Time) {}, succeeded: func(time.Time, LendingRevenueRollupStats) {},
			mismatched: func() { f.mismatched++ },
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return f.clock },
		state: &rollupState{}, retry: map[poolDay]bool{},
	}
	return f
}

func (f *fakeDayRollup) planChanges(_ context.Context, p *rollupPlan) (func(), error) {
	page := make([]fakeChange, 0, len(f.changes)-f.cursor)
	for i := f.cursor; i < len(f.changes); i++ {
		page = append(page, fakeChange{i, f.changes[i]})
	}
	last := queueChangePage(p, fakeChange{i: f.cursor - 1}, page,
		func(c fakeChange) poolDay { return c.pd }, func(c fakeChange) bool { return f.currentRows[c.pd] })
	return func() { f.cursor = last.i + 1 }, nil
}

func (f *fakeDayRollup) valueRun(ctx context.Context, c *rollupCycle, run []poolDay) (map[time.Time]bool, error) {
	if f.failRuns > 0 {
		f.failRuns--
		return nil, errors.New("store failed")
	}
	finished := map[time.Time]bool{}
	for _, pd := range run {
		f.valued = append(f.valued, pd)
		f.tasks[pd] = c.tasks[pd]
		old, exists := f.rows[pd]
		row, write, final := decideRollupDay(c, fakeRollupRows, rollupDayValue[int64]{
			key: reserveDay{pd.pool, "T", pd.day}, task: c.tasks[pd], old: old, exists: exists, current: f.currentRows[pd],
			value: func() (int64, usdDayOutcome, bool) {
				c.walk(ctx, "T", pd.day)
				if f.failing[pd] {
					return 0, usdDayDegraded, true
				}
				return f.truth[pd], usdDayValued, true
			},
		})
		if write {
			f.rows[pd] = row
			c.st.Written++
		}
		finished[pd.day] = final
	}
	return finished, nil
}

// stored fills every day as already rolled up, coverage through the span's end.
func (f *fakeDayRollup) stored() {
	for pd, v := range f.truth {
		f.rows[pd] = v
	}
	f.covs[fakePool] = rollupCoverage{floorDay: f.span.floor, throughDay: f.span.through, current: true}
}

func (f *fakeDayRollup) tryCycle() (LendingRevenueRollupStats, error) {
	f.clock = f.clock.Add(lendingRevenueRollupCycle)
	f.valued, f.tasks = nil, map[poolDay]rollupTask{}
	return f.d.cycle(context.Background())
}

func (f *fakeDayRollup) cycle(t *testing.T) LendingRevenueRollupStats {
	t.Helper()
	st, err := f.tryCycle()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (f *fakeDayRollup) through() time.Time { return f.covs[fakePool].throughDay }

func TestDayRollup_LockHeldDoesNoWork(t *testing.T) {
	f := newFakeDayRollup(10)
	f.held = true
	if st := f.cycle(t); st.Held || f.spanReads != 0 || len(f.valued) != 0 || len(f.covs) != 0 {
		t.Fatalf("held elsewhere: stats %+v, span reads %d, valued %v, coverage %v", st, f.spanReads, f.valued, f.covs)
	}
	f.held = false
	if st := f.cycle(t); !st.Held || len(f.valued) == 0 {
		t.Fatalf("lock free: stats %+v, valued %v", st, f.valued)
	}
}

func TestDayRollup_SelfFillAdvancesByChunks(t *testing.T) {
	f := newFakeDayRollup(100)
	for c, want := range []int{29, 59, 89, 99} {
		f.cycle(t)
		if !f.through().Equal(rollupDay(want)) {
			t.Fatalf("cycle %d: coverage through %v, want day %d", c, f.through(), want)
		}
	}
	for i := range 100 {
		if f.rows[poolDay{fakePool, rollupDay(i)}] != int64(i)+1 {
			t.Fatalf("day %d not filled", i)
		}
	}
}

func TestDayRollup_FailedDayCapsCoverageAndIsRetried(t *testing.T) {
	f := newFakeDayRollup(100)
	bad := poolDay{fakePool, rollupDay(5)}
	f.failing[bad] = true
	if st := f.cycle(t); st.Degraded != 1 {
		t.Fatalf("degraded %d, want 1", st.Degraded)
	}
	if _, ok := f.rows[bad]; ok || !f.through().Equal(rollupDay(4)) || !f.d.retry[bad] {
		t.Fatalf("failed day: stored %v, coverage %v, retry %v", ok, f.through(), f.d.retry[bad])
	}
	delete(f.failing, bad)
	f.cycle(t)
	if f.rows[bad] != 6 || f.d.retry[bad] || f.through().Before(rollupDay(29)) {
		t.Fatalf("retried day: stored %d, retry %v, coverage %v", f.rows[bad], f.d.retry[bad], f.through())
	}
}

func TestDayRollup_ChangeRevaluesExplained(t *testing.T) {
	f := newFakeDayRollup(20)
	f.stored()
	day, current := poolDay{fakePool, rollupDay(10)}, poolDay{fakePool, rollupDay(12)}
	f.truth[day], f.truth[current] = 999, 777
	f.currentRows[current] = true
	f.changes = []poolDay{day, current}
	st := f.cycle(t)
	if f.rows[day] != 999 || !f.tasks[day].changeOnly || !f.tasks[day].explained || st.Mismatches != 0 || f.cursor != 2 {
		t.Fatalf("change: stored %d task %+v stats %+v cursor %d", f.rows[day], f.tasks[day], st, f.cursor)
	}
	if _, queued := f.tasks[current]; queued || f.rows[current] != 13 {
		t.Fatalf("a change already held was queued: %+v stored %d", f.tasks[current], f.rows[current])
	}
}

// A change-only day whose stored row became current by the time it runs is skipped, not re-valued.
func TestDayRollup_ChangeOnlyCurrentRowIsSkipped(t *testing.T) {
	f := newFakeDayRollup(20)
	f.stored()
	day := poolDay{fakePool, rollupDay(10)}
	f.truth[day] = 999
	f.changes = []poolDay{day}
	f.d.src.changes = func(ctx context.Context, p *rollupPlan) (func(), error) {
		commit, err := f.planChanges(ctx, p)
		f.currentRows[day] = true
		return commit, err
	}
	if st := f.cycle(t); st.Skipped != 1 || f.rows[day] != 11 {
		t.Fatalf("stats %+v stored %d", st, f.rows[day])
	}
}

func TestDayRollup_SweepMismatchRewritesAndCounts(t *testing.T) {
	f := newFakeDayRollup(20)
	f.stored()
	day := poolDay{fakePool, rollupDay(3)}
	f.truth[day] = -1
	for c := range 3 {
		if f.cycle(t); f.mismatched != 0 || f.rows[day] == -1 {
			t.Fatalf("cycle %d swept day 3 early", c)
		}
	}
	st := f.cycle(t)
	if st.Mismatches != 1 || f.mismatched != 1 || st.Written != 1 || f.rows[day] != -1 || !f.tasks[day].sweep || f.tasks[day].explained {
		t.Fatalf("sweep day: stats %+v mismatched %d stored %d task %+v", st, f.mismatched, f.rows[day], f.tasks[day])
	}
	if st := f.cycle(t); st.Mismatches != 0 || f.mismatched != 1 {
		t.Fatalf("a rewritten day mismatched again: %+v", st)
	}
}

func TestDayRollup_BudgetCutChangesCarryOver(t *testing.T) {
	f := newFakeDayRollup(2000)
	f.stored()
	for i := range 1000 {
		f.changes = append(f.changes, poolDay{fakePool, rollupDay(i * 2)})
	}
	seen := map[poolDay]bool{}
	for c := 0; f.cursor < len(f.changes); c++ {
		if c == 10 {
			t.Fatalf("changes stalled at %d of %d", f.cursor, len(f.changes))
		}
		before := f.cursor
		st := f.cycle(t)
		if st.PoolDays > lendingRevenueRollupPoolDays || f.cursor == before {
			t.Fatalf("cycle %d: %d pool-days, cursor %d -> %d", c, st.PoolDays, before, f.cursor)
		}
		if c == 0 && f.cursor >= lendingRevenueRollupForcedDays {
			t.Fatalf("first cycle took %d changes past the forced share", f.cursor)
		}
		for _, pd := range f.valued {
			seen[pd] = true
		}
	}
	for _, pd := range f.changes {
		if !seen[pd] {
			t.Fatalf("change %v never valued", pd.day)
		}
	}
}

// A cycle whose store fails commits nothing of its change position, so the changes it did not store come back next cycle.
func TestDayRollup_FailedStoreLeavesChangeCursor(t *testing.T) {
	f := newFakeDayRollup(20)
	f.stored()
	a, b := poolDay{fakePool, rollupDay(10)}, poolDay{fakePool, rollupDay(12)}
	f.truth[a], f.truth[b] = 100, 200
	f.changes = []poolDay{a, b}
	f.failRuns = 1
	if _, err := f.tryCycle(); err == nil {
		t.Fatal("a failed store did not fail the cycle")
	}
	if f.cursor != 0 || f.rows[a] != 11 || f.rows[b] != 13 {
		t.Fatalf("after a failed store: cursor %d rows %d %d", f.cursor, f.rows[a], f.rows[b])
	}
	f.cycle(t)
	if f.cursor != 2 || f.rows[a] != 100 || f.rows[b] != 200 || !f.tasks[a].changeOnly || !f.tasks[b].changeOnly {
		t.Fatalf("next cycle: cursor %d rows %d %d tasks %+v %+v", f.cursor, f.rows[a], f.rows[b], f.tasks[a], f.tasks[b])
	}
}

// Exclusions are read from a day before the bootstrap, then from each completed cycle's start.
func TestDayRollup_ExclusionsReadSinceLastCycle(t *testing.T) {
	f := newFakeDayRollup(20)
	f.cycle(t)
	first := f.clock.UTC()
	f.cycle(t)
	if len(f.since) != 2 || !f.since[0].Equal(first.Add(-day24)) || !f.since[1].Equal(first) {
		t.Fatalf("exclusions read since %v, want [%v %v]", f.since, first.Add(-day24), first)
	}
}

// started is called once when the worker starts, succeeded after each cycle that completed and never after a failed one.
func TestDayRollup_RunReportsStartAndEachSuccess(t *testing.T) {
	f := newFakeDayRollup(20)
	f.d.src.budget.cycle = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var started []time.Time
	readies, succeeded := 0, 0
	f.d.src.started = func(at time.Time) { started = append(started, at) }
	f.d.src.ready = func() error {
		if readies++; readies == 2 {
			return errors.New("not ready")
		}
		return nil
	}
	f.d.src.succeeded = func(at time.Time, st LendingRevenueRollupStats) {
		if succeeded++; !st.Held || !at.Equal(f.clock) {
			t.Errorf("succeeded at %v with %+v", at, st)
		}
		if succeeded == 2 {
			cancel()
		}
	}
	done := make(chan struct{})
	go func() { f.d.run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
	if len(started) != 1 || !started[0].Equal(f.clock) || readies != 3 || succeeded != 2 {
		t.Fatalf("started %v, cycles %d, succeeded %d; want one start, 3 cycles, 2 successes", started, readies, succeeded)
	}
}

func TestDayRollup_ReadyErrorTakesNoLock(t *testing.T) {
	f := newFakeDayRollup(10)
	locked := false
	f.d.src.ready = func() error { return errors.New("not ready") }
	f.d.src.lock = func(context.Context, string, func(), *slog.Logger) (*timescale.InstanceLock, error) {
		locked = true
		return nil, nil
	}
	if _, err := f.d.cycle(context.Background()); err == nil || locked {
		t.Fatalf("err %v locked %v", err, locked)
	}
}
