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
// keyset list, and nothing of revenue's reads, valuer or tables.
type fakeDayRollup struct {
	span       rollupSpan
	covs       map[string]rollupCoverage
	rows       map[poolDay]int64 // stored values
	truth      map[poolDay]int64 // what valuing a day gives now
	failing    map[poolDay]bool  // valued with no final outcome
	changes    []poolDay
	cursor     int // changes already got past
	held       bool
	spanReads  int
	valued     []poolDay // this cycle's valuations, in order
	tasks      map[poolDay]rollupTask
	mismatches int
	d          *dayRollup
}

const fakePool = "P"

func newFakeDayRollup(days int) *fakeDayRollup {
	f := &fakeDayRollup{
		span: rollupSpan{floor: rollupDay(0), through: rollupDay(days - 1)}, covs: map[string]rollupCoverage{},
		rows: map[poolDay]int64{}, truth: map[poolDay]int64{}, failing: map[poolDay]bool{},
	}
	for i := range days {
		f.truth[poolDay{fakePool, rollupDay(i)}] = int64(i) + 1
	}
	clock := rollupDay(days + 10)
	f.d = &dayRollup{
		src: dayRollupSource{
			name: "fake rollup", lockName: "fake", ready: func() error { return nil },
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
			bootstrap: func(context.Context, time.Time) (bool, error) { return false, nil },
			changes: func(_ context.Context, p *rollupPlan) (func(), error) {
				next := f.cursor
				for next < len(f.changes) && p.add(f.changes[next], rollupTask{changeOnly: true}) {
					next++
				}
				return func() { f.cursor = next }, nil
			},
			stale:                  func(context.Context, *rollupPlan) error { return nil },
			exclusionsChangedSince: func(context.Context, time.Time) ([][2]time.Time, error) { return nil, nil },
			valueRun:               f.valueRun,
			storeCoverage: func(_ context.Context, pool string, sp rollupSpan, through time.Time) error {
				f.covs[pool] = rollupCoverage{floorDay: sp.floor, throughDay: through, current: true}
				return nil
			},
			started: func(time.Time) {}, succeeded: func(time.Time, LendingRevenueRollupStats) {},
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return clock },
		state: &rollupState{}, retry: map[poolDay]bool{},
	}
	return f
}

func (f *fakeDayRollup) valueRun(_ context.Context, tasks map[poolDay]rollupTask, run []poolDay, _ map[reserveDay]reserveUSDRead, st *LendingRevenueRollupStats) (map[time.Time]bool, error) {
	finished := map[time.Time]bool{}
	for _, pd := range run {
		st.PoolDays++
		f.valued = append(f.valued, pd)
		f.tasks[pd] = tasks[pd]
		if f.failing[pd] {
			continue
		}
		v := f.truth[pd]
		if old, ok := f.rows[pd]; ok && old != v && tasks[pd].sweep && !tasks[pd].explained {
			f.mismatches++
			st.Mismatches++
		}
		f.rows[pd] = v
		st.Written++
		finished[pd.day] = true
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

func (f *fakeDayRollup) cycle(t *testing.T) LendingRevenueRollupStats {
	t.Helper()
	f.valued, f.tasks = nil, map[poolDay]rollupTask{}
	st, err := f.d.cycle(context.Background())
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
	f.cycle(t)
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
	day := poolDay{fakePool, rollupDay(10)}
	f.truth[day] = 999
	f.changes = []poolDay{day}
	st := f.cycle(t)
	if f.rows[day] != 999 || !f.tasks[day].changeOnly || !f.tasks[day].explained || st.Mismatches != 0 || f.cursor != 1 {
		t.Fatalf("change: stored %d task %+v stats %+v cursor %d", f.rows[day], f.tasks[day], st, f.cursor)
	}
}

func TestDayRollup_SweepMismatchRewritesAndCounts(t *testing.T) {
	f := newFakeDayRollup(20)
	f.stored()
	day := poolDay{fakePool, rollupDay(3)}
	f.truth[day] = -1
	for c := range 3 {
		if f.cycle(t); f.mismatches != 0 || f.rows[day] == -1 {
			t.Fatalf("cycle %d swept day 3 early", c)
		}
	}
	st := f.cycle(t)
	if st.Mismatches != 1 || f.rows[day] != -1 || !f.tasks[day].sweep || f.tasks[day].explained {
		t.Fatalf("sweep day: stats %+v stored %d task %+v", st, f.rows[day], f.tasks[day])
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
