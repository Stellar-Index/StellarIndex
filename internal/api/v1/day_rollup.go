package v1

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const day24 = 24 * time.Hour

// dayRollupSource is what a per-day USD rollup gives the cycle it shares with the others: its lock, its reads, its change
// detection, its valuation, its store and its metrics. Every func is required.
type dayRollupSource struct {
	name, lockName string // name prefixes the cycle's log lines
	ready          func() error
	lock           func(ctx context.Context, name string, onLost func(), logger *slog.Logger) (*timescale.InstanceLock, error)
	// spans are each pool's wholly valuable days and what its coverage row holds.
	spans func(ctx context.Context) (map[string]rollupSpan, map[string]rollupCoverage, error)
	// bootstrap starts change detection, true when it did so now: exclusions are then read from a sweep's length back.
	bootstrap func(ctx context.Context, started time.Time) (bool, error)
	// changes queues the changed days; commit advances its position once the cycle has executed them.
	changes                func(ctx context.Context, p *rollupPlan) (commit func(), err error)
	stale                  func(ctx context.Context, p *rollupPlan) error
	exclusionsChangedSince func(ctx context.Context, since time.Time) ([][2]time.Time, error)
	// valueRun values one run of a pool's consecutive queued days and returns the days that reached a final outcome.
	valueRun      func(ctx context.Context, tasks map[poolDay]rollupTask, run []poolDay, walks map[reserveDay]reserveUSDRead, st *LendingRevenueRollupStats) (map[time.Time]bool, error)
	storeCoverage func(ctx context.Context, pool string, sp rollupSpan, through time.Time) error
	started       func(at time.Time)
	succeeded     func(at time.Time, st LendingRevenueRollupStats)
}

// rollupCoverage is what a pool's coverage row holds: its days through throughDay from floorDay, current when stored at
// the valuer's revision.
type rollupCoverage struct {
	floorDay, throughDay time.Time
	current              bool
}

// rollupState is the planning state a cycle commits once it has executed. It is in memory; after a restart the daily
// sweep re-proves everything.
type rollupState struct {
	exclusionsSince time.Time
	windows         []exclusionWindow // exclusion windows not yet wholly re-valued
	sweepDay        time.Time
}

// dayRollup is the cycle every per-day USD rollup runs: the sweep's share, retries, changes, stale revisions, changed
// exclusions, the trailing days, then the self-fill, under one lock and one budget.
type dayRollup struct {
	src    dayRollupSource
	logger *slog.Logger
	now    func() time.Time
	state  *rollupState
	retry  map[poolDay]bool // days whose last valuation reached no final outcome, kept until one does
}

type poolDay struct {
	pool string
	day  time.Time
}

type reserveDay struct {
	pool, asset string
	day         time.Time
}

// exclusionWindow is a changed source exclusion's [from, until) and the first day not yet queued.
type exclusionWindow struct {
	from, until, next time.Time
}

// rollupTask is why a pool-day is valued: a change-only task skips a day whose stored row is already current, and a
// difference on a day only the sweep queued is a mismatch (any other reason explains it).
type rollupTask struct {
	changeOnly, sweep, fill, explained bool
}

// rollupSpan is a pool's wholly valuable UTC days: from floor through through, and a pending redo's first day.
type rollupSpan struct {
	floor, through, redo time.Time
}

func (sp rollupSpan) holds(d time.Time) bool { return !d.Before(sp.floor) && !d.After(sp.through) }

// run cycles until ctx ends; a failed cycle is logged and the next one retries.
func (d *dayRollup) run(ctx context.Context) {
	d.src.started(d.now())
	t := time.NewTicker(lendingRevenueRollupCycle)
	defer t.Stop()
	for {
		d.runCycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// runCycle runs one cycle under a budget of the worker's own, cancelled when ctx ends.
func (d *dayRollup) runCycle(ctx context.Context) {
	cctx, cancel := context.WithTimeout(context.Background(), lendingRevenueRollupCycleTimeout)
	defer cancel()
	defer context.AfterFunc(ctx, cancel)()
	st, err := d.cycle(cctx) //nolint:contextcheck // the cycle's budget is the worker's own; AfterFunc ties it to ctx
	switch {
	case err != nil && ctx.Err() == nil:
		d.logger.Warn(d.src.name+" cycle failed", "err", err)
	case err == nil && st.Held:
		d.src.succeeded(d.now(), st)
		d.logger.Info(d.src.name+" cycle", "pool_days", st.PoolDays, "written", st.Written,
			"mismatches", st.Mismatches, "degraded", st.Degraded, "refused", st.Refused)
	}
}

// cycle is one pass under the lock, each step within its share of [lendingRevenueRollupPoolDays]; [dayRollup.run]
// bounds it.
func (d *dayRollup) cycle(ctx context.Context) (LendingRevenueRollupStats, error) {
	var st LendingRevenueRollupStats
	if err := d.src.ready(); err != nil {
		return st, err
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lock, err := d.src.lock(cctx, d.src.lockName, cancel, d.logger)
	if errors.Is(err, timescale.ErrInstanceLockHeld) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	defer d.release(lock) //nolint:contextcheck // must unlock after ctx is cancelled
	st.Held = true
	p, commit, err := d.plan(cctx, d.now().UTC())
	if err != nil {
		return st, err
	}
	if err := d.execute(cctx, p, &st); err != nil {
		return st, err
	}
	commit()
	*d.state = rollupState{exclusionsSince: p.exclusionsSince, windows: p.windows, sweepDay: p.sweepDay}
	return st, nil
}

// release frees the lock on a context of its own, so a cancelled or timed-out cycle still unlocks.
func (d *dayRollup) release(lock *timescale.InstanceLock) {
	ctx, cancel := context.WithTimeout(context.Background(), lendingRevenueRollupReleaseTimeout)
	defer cancel()
	if err := lock.Release(ctx); err != nil {
		d.logger.Warn(d.src.name+": lock release", "err", err)
	}
}

// rollupPlan is one cycle's queue of pool-days, in queue order and within limit, and the planning state the cycle
// commits only once it has executed.
type rollupPlan struct {
	spans  map[string]rollupSpan
	pools  []string // spans' keys, sorted
	covs   map[string]rollupCoverage
	tasks  map[poolDay]rollupTask
	order  []poolDay
	filled map[string]time.Time // each pool's last self-fill day queued
	limit  int

	exclusionsSince time.Time
	windows         []exclusionWindow
	sweepDay        time.Time
}

// add queues pd, merging with a task already queued for it; false when the budget is spent.
func (p *rollupPlan) add(pd poolDay, t rollupTask) bool {
	sp, ok := p.spans[pd.pool]
	if !ok || !sp.holds(pd.day) {
		return true
	}
	t.explained = !t.sweep
	if old, seen := p.tasks[pd]; seen {
		old.changeOnly = old.changeOnly && t.changeOnly
		old.sweep, old.fill, old.explained = old.sweep || t.sweep, old.fill || t.fill, old.explained || t.explained
		p.tasks[pd] = old
		return true
	}
	if len(p.order) >= p.limit {
		return false
	}
	p.tasks[pd] = t
	p.order = append(p.order, pd)
	return true
}

// plan queues the cycle's pool-days in priority order; the forced share's cap leaves the trailing days and the self-fill
// room in every cycle.
func (d *dayRollup) plan(ctx context.Context, started time.Time) (*rollupPlan, func(), error) {
	spans, covs, err := d.src.spans(ctx)
	if err != nil {
		return nil, nil, err
	}
	booted, err := d.src.bootstrap(ctx, started)
	if err != nil {
		return nil, nil, err
	}
	if booted {
		d.state.exclusionsSince = started.Add(-day24)
	}
	p := &rollupPlan{
		spans: spans, covs: covs, tasks: map[poolDay]rollupTask{}, filled: map[string]time.Time{}, limit: lendingRevenueRollupForcedDays,
		exclusionsSince: d.state.exclusionsSince, windows: slices.Clone(d.state.windows), sweepDay: d.state.sweepDay,
	}
	for pool := range spans {
		p.pools = append(p.pools, pool)
	}
	sort.Strings(p.pools)
	p.planSweep()
	d.planRetries(p)
	commit, err := d.src.changes(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	if err := d.src.stale(ctx, p); err != nil {
		return nil, nil, err
	}
	if err := d.planExclusions(ctx, p, started); err != nil {
		return nil, nil, err
	}
	p.limit = lendingRevenueRollupTrailingDays
	for _, pool := range p.pools {
		for i := range lendingRevenueRollupRewalkDays {
			p.add(poolDay{pool, spans[pool].through.Add(-time.Duration(i) * day24)}, rollupTask{})
		}
	}
	p.limit = lendingRevenueRollupPoolDays
	for _, pool := range p.pools {
		p.planFill(pool, spans[pool])
	}
	return p, commit, nil
}

// planRetries queues the days whose last valuation did not finish; one that left the span is the self-fill's again.
func (d *dayRollup) planRetries(p *rollupPlan) {
	for pd := range d.retry {
		if sp, ok := p.spans[pd.pool]; !ok || !sp.holds(pd.day) {
			delete(d.retry, pd)
			continue
		}
		p.add(pd, rollupTask{})
	}
}

// planExclusions takes the source exclusions created or lifted since the last read as windows to re-value, then queues
// the windows' stored days in order; a window the budget cuts resumes there next cycle.
func (d *dayRollup) planExclusions(ctx context.Context, p *rollupPlan, started time.Time) error {
	wins, err := d.src.exclusionsChangedSince(ctx, p.exclusionsSince)
	if err != nil {
		return err
	}
	for _, w := range wins {
		until := w[1]
		if until.IsZero() {
			until = started
		}
		from := w[0].UTC().Truncate(day24)
		p.windows = append(p.windows, exclusionWindow{from: from, until: until, next: from})
	}
	p.exclusionsSince = started
	for len(p.windows) > 0 {
		if !p.addWindow(&p.windows[0]) {
			return nil
		}
		p.windows = p.windows[1:]
	}
	return nil
}

// addWindow queues w's stored days from w.next, day by day across the pools; false (w.next at the cut day) when the
// budget is spent.
func (p *rollupPlan) addWindow(w *exclusionWindow) bool {
	var first, last time.Time
	for i, pool := range p.pools {
		sp, through := p.spans[pool], p.storedThrough(pool)
		if i == 0 || sp.floor.Before(first) {
			first = sp.floor
		}
		last = maxTime(last, through)
	}
	for d := maxTime(w.next, first); d.Before(w.until) && !d.After(last); d = d.Add(day24) {
		for _, pool := range p.pools {
			if d.Before(p.spans[pool].floor) || d.After(p.storedThrough(pool)) {
				continue
			}
			if !p.add(poolDay{pool, d}, rollupTask{}) {
				w.next = d
				return false
			}
		}
	}
	return true
}

// storedThrough is the last day of pool's span its coverage holds.
func (p *rollupPlan) storedThrough(pool string) time.Time {
	last := p.spans[pool].through
	if c, ok := p.covs[pool]; ok {
		last = minTime(last, c.throughDay)
	}
	return last
}

// planSweep queues this cycle's share of every stored span, all pools on the same days so one walk serves them all.
func (p *rollupPlan) planSweep() {
	lo, hi, ok := p.storedBounds()
	if !ok {
		return
	}
	n := int(hi.Sub(lo)/day24) + 1
	k := (n + lendingRevenueRollupSweepCycles - 1) / lendingRevenueRollupSweepCycles
	if p.sweepDay.Before(lo) || p.sweepDay.After(hi) {
		p.sweepDay = lo
	}
	for i := 0; i < k && !p.sweepDay.After(hi); i++ {
		for _, pool := range p.pools {
			if c, ok := p.covs[pool]; ok && !p.sweepDay.After(c.throughDay) {
				p.add(poolDay{pool, p.sweepDay}, rollupTask{sweep: true})
			}
		}
		p.sweepDay = p.sweepDay.Add(day24)
	}
}

// storedBounds is the first and last day any pool's coverage holds, false when none holds a day.
func (p *rollupPlan) storedBounds() (lo, hi time.Time, ok bool) {
	for pool, sp := range p.spans {
		c, ok := p.covs[pool]
		if !ok || c.throughDay.Before(sp.floor) {
			continue
		}
		if lo.IsZero() || sp.floor.Before(lo) {
			lo = sp.floor
		}
		if t := minTime(c.throughDay, sp.through); t.After(hi) {
			hi = t
		}
	}
	return lo, hi, !lo.IsZero() && !hi.Before(lo)
}

// planFill queues the next chunk of pool's never-stored days, past its coverage when that is of this floor and revision.
func (p *rollupPlan) planFill(pool string, sp rollupSpan) {
	from := sp.floor
	if c, ok := p.covs[pool]; ok && c.floorDay.Equal(sp.floor) && c.current {
		from = minTime(c.throughDay, sp.through).Add(day24)
	}
	p.filled[pool] = from.Add(-day24)
	for d := from; !d.After(sp.through) && d.Before(from.Add(lendingRevenueRollupFillChunk*day24)); d = d.Add(day24) {
		if !p.add(poolDay{pool, d}, rollupTask{fill: true}) {
			return
		}
		p.filled[pool] = d
	}
}

// execute values the queued days pool by pool, storing each pool's coverage once its days are done, so a slow pool's
// fill is kept even when a later pool fails.
func (d *dayRollup) execute(ctx context.Context, p *rollupPlan, st *LendingRevenueRollupStats) error {
	byPool := map[string][]poolDay{}
	for _, pd := range p.order {
		byPool[pd.pool] = append(byPool[pd.pool], pd)
	}
	walks := map[reserveDay]reserveUSDRead{} // one walk per (asset, day) per cycle, whatever pools hold the asset
	for _, pool := range p.pools {
		unfinished, err := d.executePool(ctx, p.tasks, byPool[pool], walks, st)
		if err != nil {
			return err
		}
		if err := d.storeCoverage(ctx, p, pool, unfinished); err != nil {
			return err
		}
	}
	return nil
}

// executePool values one pool's queued days in runs of consecutive days and returns its first fill day that reached no
// final outcome (zero: none).
func (d *dayRollup) executePool(ctx context.Context, tasks map[poolDay]rollupTask, days []poolDay, walks map[reserveDay]reserveUSDRead, st *LendingRevenueRollupStats) (time.Time, error) {
	sort.Slice(days, func(i, j int) bool { return days[i].day.Before(days[j].day) })
	var unfinished time.Time
	for i := 0; i < len(days); {
		j := i + 1
		for j < len(days) && j-i < lendingRevenueRollupMaxRun && days[j].day.Sub(days[i].day) < lendingRevenueRollupMaxRun*day24 {
			j++
		}
		finished, err := d.src.valueRun(ctx, tasks, days[i:j], walks, st)
		if err != nil {
			return unfinished, err
		}
		for _, pd := range days[i:j] {
			if finished[pd.day] {
				delete(d.retry, pd)
				continue
			}
			d.retry[pd] = true
			if tasks[pd].fill && unfinished.IsZero() {
				unfinished = pd.day
			}
		}
		i = j
	}
	return unfinished, nil
}

// storeCoverage stores pool's coverage through its last fill day, capped before the first fill day left unfinished, so
// inside coverage a missing row only ever means a day with nothing to value.
func (d *dayRollup) storeCoverage(ctx context.Context, p *rollupPlan, pool string, unfinished time.Time) error {
	through := p.filled[pool]
	if !unfinished.IsZero() && !unfinished.After(through) {
		through = unfinished.Add(-day24)
	}
	return d.src.storeCoverage(ctx, pool, p.spans[pool], through)
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
