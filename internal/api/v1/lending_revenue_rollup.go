package v1

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The rollup's cadence and its per-cycle work budget.
const (
	lendingRevenueRollupCycle        = 10 * time.Minute
	lendingRevenueRollupCycleTimeout = 8 * time.Minute
	// lendingRevenueRollupPoolDays bounds the (pool, day) valuations one cycle makes; the sweep's share comes first.
	lendingRevenueRollupPoolDays = 600
	// The sweep, retries, changes, stale rows and exclusions fill the queue to ForcedDays, the trailing days to
	// TrailingDays, and the self-fill the rest, so each always advances.
	lendingRevenueRollupForcedDays     = lendingRevenueRollupPoolDays * 2 / 3
	lendingRevenueRollupTrailingDays   = lendingRevenueRollupPoolDays * 5 / 6
	lendingRevenueRollupFillChunk      = 30  // days per self-fill read
	lendingRevenueRollupRewalkDays     = 3   // trailing days re-walked every cycle, for late price buckets
	lendingRevenueRollupSweepCycles    = 144 // cycles per full sweep: one UTC day at the cadence
	lendingRevenueRollupChangeSlack    = 10 * time.Minute
	lendingRevenueRollupChangeLimit    = 5000
	lendingRevenueRollupMaxRun         = 31 // days per lake read
	lendingRevenueRollupReleaseTimeout = 5 * time.Second
)

const day24 = 24 * time.Hour

// LendingRevenueRollupStore is the Postgres side of the rollup.
type LendingRevenueRollupStore interface {
	HoldInstanceLock(ctx context.Context, name string, onLost func(), logger *slog.Logger) (*timescale.InstanceLock, error)
	LendingRevenueUSDDays(ctx context.Context, pool string, from, through time.Time) ([]timescale.LendingRevenueUSDDay, error)
	UpsertLendingRevenueUSDDays(ctx context.Context, rows []timescale.LendingRevenueUSDDay) error
	LendingRevenueUSDStaleRevDays(ctx context.Context, valuationRev, deriveRev int16, limit int) (map[string][]time.Time, error)
	LendingRevenueUSDNewestDerivedAt(ctx context.Context) (time.Time, error)
	LendingRevenueUSDCoverages(ctx context.Context) (map[string]timescale.LendingRevenueUSDCoverage, error)
	UpsertLendingRevenueUSDCoverage(ctx context.Context, c timescale.LendingRevenueUSDCoverage) error
	SourceExclusionWindowsChangedSince(ctx context.Context, since time.Time) ([][2]time.Time, error)
}

// lendingRevenueRollupLake is the ClickHouse side: the /revenue read itself, plus change detection.
type lendingRevenueRollupLake interface {
	lendingRevenueReader
	LendingInterestChangedDays(ctx context.Context, after clickhouse.LendingInterestDay, limit int) ([]clickhouse.LendingInterestDay, error)
	LendingInterestPoolDays(ctx context.Context, pool string, from, before time.Time) ([]clickhouse.LendingInterestDay, error)
	LendingInterestPools(ctx context.Context) ([]string, error)
}

// LendingRevenueRollupStats is what one cycle did.
type LendingRevenueRollupStats struct {
	Held                                  bool // false: another process holds the lock; the cycle did nothing
	PoolDays, Written, Unchanged, Skipped int
	Mismatches, Degraded, Refused         int
}

// LendingRevenueRollup keeps lending_revenue_usd_daily: one row per (pool, asset, UTC day), each hour valued at the price a
// walk of that UTC day gives. Its state is in memory; after a restart the daily sweep re-proves everything.
type LendingRevenueRollup struct {
	store  LendingRevenueRollupStore
	lake   lendingRevenueRollupLake // nil when the explorer reader has no lending interest reads
	walk   func(ctx context.Context, asset string, day time.Time) reserveUSDRead
	logger *slog.Logger
	now    func() time.Time

	cursor          clickhouse.LendingInterestDay // change detection's keyset position; zero DerivedAt until bootstrapped
	exclusionsSince time.Time
	windows         []exclusionWindow // exclusion windows not yet wholly re-valued
	sweepDay        time.Time
	retry           map[poolDay]bool // days whose last valuation reached no final outcome, kept until one does
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

// NewLendingRevenueRollup builds the rollup over s's lake reader and valuer.
func (s *Server) NewLendingRevenueRollup(store LendingRevenueRollupStore, logger *slog.Logger) *LendingRevenueRollup {
	lake, _ := s.Explorer.(lendingRevenueRollupLake)
	walk := func(ctx context.Context, asset string, day time.Time) reserveUSDRead {
		return s.revenueDayRead(ctx, asset, day, lendingRevenueDayWalkBudget)
	}
	return &LendingRevenueRollup{store: store, lake: lake, walk: walk, logger: logger, now: time.Now, retry: map[poolDay]bool{}}
}

// Run cycles until ctx ends; a failed cycle is logged and the next one retries.
func (r *LendingRevenueRollup) Run(ctx context.Context) {
	obs.LendingRevenueRollupStartedUnix.Set(float64(r.now().Unix()))
	t := time.NewTicker(lendingRevenueRollupCycle)
	defer t.Stop()
	for {
		r.runCycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// runCycle runs one cycle under a budget of the worker's own, cancelled when ctx ends.
func (r *LendingRevenueRollup) runCycle(ctx context.Context) {
	cctx, cancel := context.WithTimeout(context.Background(), lendingRevenueRollupCycleTimeout)
	defer cancel()
	defer context.AfterFunc(ctx, cancel)()
	st, err := r.Cycle(cctx) //nolint:contextcheck // the cycle's budget is the worker's own; AfterFunc ties it to ctx
	switch {
	case err != nil && ctx.Err() == nil:
		r.logger.Warn("lending revenue rollup cycle failed", "err", err)
	case err == nil && st.Held:
		obs.LendingRevenueRollupLastSuccessUnix.Set(float64(r.now().Unix()))
		obs.LendingRevenueRollupDaysNotValued.WithLabelValues("degraded").Set(float64(st.Degraded))
		obs.LendingRevenueRollupDaysNotValued.WithLabelValues("refused").Set(float64(st.Refused))
		r.logger.Info("lending revenue rollup cycle", "pool_days", st.PoolDays, "written", st.Written,
			"mismatches", st.Mismatches, "degraded", st.Degraded, "refused", st.Refused)
	}
}

// rollupSpan is a pool's wholly valuable UTC days, clipped as /revenue clips: from the first day starting at or after the
// derive floor's close, through the last day ending by the coverage's next hour and a pending redo's first suspect hour.
type rollupSpan struct {
	floor, through, redo time.Time
}

func revenueRollupSpan(rev clickhouse.LendingRevenue) (rollupSpan, bool) {
	if !rev.Covered || rev.FloorLedger == 0 {
		return rollupSpan{}, false
	}
	sp := rollupSpan{floor: rev.FloorTime.UTC().Truncate(day24)}
	if sp.floor.Before(rev.FloorTime) {
		sp.floor = sp.floor.Add(day24)
	}
	before := rev.ThroughHour.Add(time.Hour)
	if rev.Redo != nil {
		sp.redo = rev.RedoHour.UTC().Truncate(day24)
		before = minTime(before, rev.RedoHour)
	}
	sp.through = before.UTC().Truncate(day24).Add(-day24)
	return sp, !sp.through.Before(sp.floor)
}

func (sp rollupSpan) holds(d time.Time) bool { return !d.Before(sp.floor) && !d.After(sp.through) }

// rollupPlan is one cycle's queue of pool-days, in queue order and within limit, and the planning state the cycle
// commits only once it has executed.
type rollupPlan struct {
	spans  map[string]rollupSpan
	pools  []string // spans' keys, sorted
	covs   map[string]timescale.LendingRevenueUSDCoverage
	tasks  map[poolDay]rollupTask
	order  []poolDay
	filled map[string]time.Time // each pool's last self-fill day queued
	limit  int

	cursor          clickhouse.LendingInterestDay
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

// Cycle is one pass under the lock: the sweep's share, retries, changed derives, stale revisions, changed exclusions, the
// trailing days, then the self-fill, each within its share of [lendingRevenueRollupPoolDays]. [Run] bounds it.
func (r *LendingRevenueRollup) Cycle(ctx context.Context) (LendingRevenueRollupStats, error) {
	var st LendingRevenueRollupStats
	if r.lake == nil {
		return st, errors.New("lending revenue rollup: the explorer reader has no lending interest reads")
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lock, err := r.store.HoldInstanceLock(cctx, timescale.LendingRevenueRollupLockName, cancel, r.logger)
	if errors.Is(err, timescale.ErrInstanceLockHeld) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	defer r.release(lock) //nolint:contextcheck // must unlock after ctx is cancelled
	st.Held = true
	p, err := r.plan(cctx, r.now().UTC())
	if err != nil {
		return st, err
	}
	if err := r.execute(cctx, p, &st); err != nil {
		return st, err
	}
	r.cursor, r.exclusionsSince, r.windows, r.sweepDay = p.cursor, p.exclusionsSince, p.windows, p.sweepDay
	return st, nil
}

// release frees the lock on a context of its own, so a cancelled or timed-out cycle still unlocks.
func (r *LendingRevenueRollup) release(lock *timescale.InstanceLock) {
	ctx, cancel := context.WithTimeout(context.Background(), lendingRevenueRollupReleaseTimeout)
	defer cancel()
	if err := lock.Release(ctx); err != nil {
		r.logger.Warn("lending revenue rollup: lock release", "err", err)
	}
}

// plan queues the cycle's pool-days in priority order; the forced share's cap leaves the trailing days and the self-fill
// room in every cycle.
func (r *LendingRevenueRollup) plan(ctx context.Context, started time.Time) (*rollupPlan, error) {
	spans, covs, err := r.spans(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.bootstrap(ctx, started); err != nil {
		return nil, err
	}
	p := &rollupPlan{
		spans: spans, covs: covs, tasks: map[poolDay]rollupTask{}, filled: map[string]time.Time{}, limit: lendingRevenueRollupForcedDays,
		cursor: r.cursor, exclusionsSince: r.exclusionsSince, windows: slices.Clone(r.windows), sweepDay: r.sweepDay,
	}
	for pool := range spans {
		p.pools = append(p.pools, pool)
	}
	sort.Strings(p.pools)
	p.planSweep()
	r.planRetries(p)
	if err := r.planChanges(ctx, p); err != nil {
		return nil, err
	}
	if err := r.planStale(ctx, p); err != nil {
		return nil, err
	}
	if err := r.planExclusions(ctx, p, started); err != nil {
		return nil, err
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
	return p, nil
}

// spans reads each pool's coverage through the /revenue read itself, over an empty window so no row is read.
func (r *LendingRevenueRollup) spans(ctx context.Context) (map[string]rollupSpan, map[string]timescale.LendingRevenueUSDCoverage, error) {
	pools, err := r.lake.LendingInterestPools(ctx)
	if err != nil {
		return nil, nil, err
	}
	covs, err := r.store.LendingRevenueUSDCoverages(ctx)
	if err != nil {
		return nil, nil, err
	}
	at := r.now().UTC().Truncate(time.Hour)
	spans := map[string]rollupSpan{}
	for _, pool := range pools {
		rev, err := r.lake.LendingPoolRevenue(ctx, pool, "", clickhouse.LendingHistoryQuery{From: at, To: at})
		if err != nil {
			return nil, nil, fmt.Errorf("lending revenue rollup: coverage of %s: %w", pool, err)
		}
		if sp, ok := revenueRollupSpan(rev); ok {
			spans[pool] = sp
		}
	}
	return spans, covs, nil
}

// bootstrap starts change detection at the newest stored derive (an empty table is the self-fill's), and exclusions a
// sweep's length back.
func (r *LendingRevenueRollup) bootstrap(ctx context.Context, started time.Time) error {
	if !r.cursor.DerivedAt.IsZero() {
		return nil
	}
	newest, err := r.store.LendingRevenueUSDNewestDerivedAt(ctx)
	if err != nil {
		return err
	}
	if newest.IsZero() {
		newest = started
	}
	r.cursor = clickhouse.LendingInterestDay{DerivedAt: newest}
	r.exclusionsSince = started.Add(-day24)
	return nil
}

// planRetries queues the days whose last valuation did not finish; one that left the span is the self-fill's again.
func (r *LendingRevenueRollup) planRetries(p *rollupPlan) {
	for pd := range r.retry {
		if sp, ok := p.spans[pd.pool]; !ok || !sp.holds(pd.day) {
			delete(r.retry, pd)
			continue
		}
		p.add(pd, rollupTask{})
	}
}

// planChanges queues the days a derive rewrote, paging by keyset so a derive wider than the budget still moves on, and
// re-reads the slack before the cursor for derives that became visible late. A re-derive that only deletes an hour's
// superseded rows writes no newer derived_at; the daily sweep is what catches it.
func (r *LendingRevenueRollup) planChanges(ctx context.Context, p *rollupPlan) error {
	late, err := r.lake.LendingInterestChangedDays(ctx, clickhouse.LendingInterestDay{DerivedAt: p.cursor.DerivedAt.Add(-lendingRevenueRollupChangeSlack)}, lendingRevenueRollupChangeLimit)
	if err != nil {
		return err
	}
	if _, err := r.queueChanges(ctx, p, late); err != nil {
		return err
	}
	changed, err := r.lake.LendingInterestChangedDays(ctx, p.cursor, lendingRevenueRollupChangeLimit)
	if err != nil {
		return err
	}
	p.cursor, err = r.queueChanges(ctx, p, changed)
	return err
}

// queueChanges queues the changes the store does not already hold, in order, and returns the last change it got past. A
// stored row valued from that derive or a newer one costs no budget.
func (r *LendingRevenueRollup) queueChanges(ctx context.Context, p *rollupPlan, changes []clickhouse.LendingInterestDay) (clickhouse.LendingInterestDay, error) {
	last := p.cursor
	stored, err := r.storedRows(ctx, changes)
	if err != nil {
		return last, err
	}
	for _, c := range changes {
		if old, ok := stored[reserveDay{c.Pool, c.Asset, c.Day}]; !ok || !rollupRowCurrent(old, c) {
			if !p.add(poolDay{c.Pool, c.Day}, rollupTask{changeOnly: true}) {
				return last, nil
			}
		}
		last = c
	}
	return last, nil
}

// storedRows reads the stored rows of changes' days, one read per pool over its changed days' range.
func (r *LendingRevenueRollup) storedRows(ctx context.Context, changes []clickhouse.LendingInterestDay) (map[reserveDay]timescale.LendingRevenueUSDDay, error) {
	bounds := map[string][2]time.Time{}
	for _, c := range changes {
		b, ok := bounds[c.Pool]
		if !ok {
			b = [2]time.Time{c.Day, c.Day}
		}
		bounds[c.Pool] = [2]time.Time{minTime(b[0], c.Day), maxTime(b[1], c.Day)}
	}
	out := map[reserveDay]timescale.LendingRevenueUSDDay{}
	for pool, b := range bounds {
		rows, err := r.store.LendingRevenueUSDDays(ctx, pool, b[0], b[1])
		if err != nil {
			return nil, err
		}
		for _, d := range rows {
			out[reserveDay{d.Pool, d.Asset, d.Day}] = d
		}
	}
	return out, nil
}

// planStale queues the stored days of an older valuation or derive revision.
func (r *LendingRevenueRollup) planStale(ctx context.Context, p *rollupPlan) error {
	stale, err := r.store.LendingRevenueUSDStaleRevDays(ctx, lendingRevenueValuationRev, int16(lendinginterest.DeriveRev), lendingRevenueRollupPoolDays) //nolint:gosec // a small revision number
	if err != nil {
		return err
	}
	for pool, days := range stale {
		for _, d := range days {
			p.add(poolDay{pool, d}, rollupTask{})
		}
	}
	return nil
}

// planExclusions takes the source exclusions created or lifted since the last read as windows to re-value, then queues
// the windows' stored days in order; a window the budget cuts resumes there next cycle.
func (r *LendingRevenueRollup) planExclusions(ctx context.Context, p *rollupPlan, started time.Time) error {
	wins, err := r.store.SourceExclusionWindowsChangedSince(ctx, p.exclusionsSince)
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
		last = minTime(last, c.ThroughDay)
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
			if c, ok := p.covs[pool]; ok && !p.sweepDay.After(c.ThroughDay) {
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
		if !ok || c.ThroughDay.Before(sp.floor) {
			continue
		}
		if lo.IsZero() || sp.floor.Before(lo) {
			lo = sp.floor
		}
		if t := minTime(c.ThroughDay, sp.through); t.After(hi) {
			hi = t
		}
	}
	return lo, hi, !lo.IsZero() && !hi.Before(lo)
}

// planFill queues the next chunk of pool's never-stored days, past its coverage when that is of this floor and revision.
func (p *rollupPlan) planFill(pool string, sp rollupSpan) {
	from := sp.floor
	if c, ok := p.covs[pool]; ok && c.FloorDay.Equal(sp.floor) && c.ValuationRev == lendingRevenueValuationRev {
		from = minTime(c.ThroughDay, sp.through).Add(day24)
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
func (r *LendingRevenueRollup) execute(ctx context.Context, p *rollupPlan, st *LendingRevenueRollupStats) error {
	byPool := map[string][]poolDay{}
	for _, pd := range p.order {
		byPool[pd.pool] = append(byPool[pd.pool], pd)
	}
	walks := map[reserveDay]reserveUSDRead{} // one walk per (asset, day) per cycle, whatever pools hold the asset
	for _, pool := range p.pools {
		unfinished, err := r.executePool(ctx, p.tasks, byPool[pool], walks, st)
		if err != nil {
			return err
		}
		if err := r.storeCoverage(ctx, p, pool, unfinished); err != nil {
			return err
		}
	}
	return nil
}

// executePool values one pool's queued days in runs of consecutive days and returns its first fill day that reached no
// final outcome (zero: none).
func (r *LendingRevenueRollup) executePool(ctx context.Context, tasks map[poolDay]rollupTask, days []poolDay, walks map[reserveDay]reserveUSDRead, st *LendingRevenueRollupStats) (time.Time, error) {
	sort.Slice(days, func(i, j int) bool { return days[i].day.Before(days[j].day) })
	var unfinished time.Time
	for i := 0; i < len(days); {
		j := i + 1
		for j < len(days) && j-i < lendingRevenueRollupMaxRun && days[j].day.Sub(days[i].day) < lendingRevenueRollupMaxRun*day24 {
			j++
		}
		finished, err := r.executeRun(ctx, tasks, days[i:j], walks, st)
		if err != nil {
			return unfinished, err
		}
		for _, pd := range days[i:j] {
			if finished[pd.day] {
				delete(r.retry, pd)
				continue
			}
			r.retry[pd] = true
			if tasks[pd].fill && unfinished.IsZero() {
				unfinished = pd.day
			}
		}
		i = j
	}
	return unfinished, nil
}

// storeCoverage writes pool's coverage through its last fill day, capped before the first fill day left unfinished, so
// inside coverage a missing row only ever means a day with no interest rows.
func (r *LendingRevenueRollup) storeCoverage(ctx context.Context, p *rollupPlan, pool string, unfinished time.Time) error {
	sp, through := p.spans[pool], p.filled[pool]
	if !unfinished.IsZero() && !unfinished.After(through) {
		through = unfinished.Add(-day24)
	}
	c := timescale.LendingRevenueUSDCoverage{
		Pool: pool, FloorDay: sp.floor, ThroughDay: through, RedoFrom: sp.redo,
		InterestDeriveRev: int16(lendinginterest.DeriveRev), ValuationRev: lendingRevenueValuationRev, //nolint:gosec // a small revision number
	}
	return r.store.UpsertLendingRevenueUSDCoverage(ctx, c)
}

// executeRun values one run: the interest days first, then the rows (so a derive in between reads as newer next cycle),
// then the stored days, then one write, with no transaction across a read. finished holds the run's days whose every
// reserve reached a final outcome: valued, stored with its reason, refused and recorded, or proven to have no interest.
func (r *LendingRevenueRollup) executeRun(ctx context.Context, tasks map[poolDay]rollupTask, run []poolDay, walks map[reserveDay]reserveUSDRead, st *LendingRevenueRollupStats) (map[time.Time]bool, error) {
	pool, from, last := run[0].pool, run[0].day, run[len(run)-1].day
	before := last.Add(day24)
	infos, err := r.lake.LendingInterestPoolDays(ctx, pool, from, before)
	if err != nil {
		return nil, err
	}
	days := int(before.Sub(from) / day24)
	rev, err := r.lake.LendingPoolRevenue(ctx, pool, "", clickhouse.LendingHistoryQuery{From: from, To: before, Daily: true, MaxPoints: days + 1})
	if err != nil {
		return nil, err
	}
	finished := map[time.Time]bool{}
	sp, ok := revenueRollupSpan(rev)
	if !ok {
		return finished, nil
	}
	inRun := func(d time.Time) bool { return sp.holds(d) && !d.Add(day24).After(rev.Before) }
	for _, pd := range run {
		finished[pd.day] = inRun(pd.day)
	}
	stored, err := r.store.LendingRevenueUSDDays(ctx, pool, from, last)
	if err != nil {
		return nil, err
	}
	rr := rollupRun{pool: pool, have: map[reserveDay]timescale.LendingRevenueUSDDay{}, hours: map[reserveDay][]clickhouse.LendingRevenueBucket{}}
	for _, d := range stored {
		rr.have[reserveDay{pool, d.Asset, d.Day}] = d
	}
	for _, res := range rev.Reserves {
		for _, h := range res.Hours {
			k := reserveDay{pool, res.Asset, h.Start.UTC().Truncate(day24)}
			rr.hours[k] = append(rr.hours[k], h)
		}
	}
	var writes []timescale.LendingRevenueUSDDay
	for _, info := range infos {
		t, queued := tasks[poolDay{pool, info.Day}]
		if !queued || !inRun(info.Day) {
			continue
		}
		row, write, final := r.valueDay(ctx, &rr, t, info, walks, st)
		finished[info.Day] = finished[info.Day] && final
		if write {
			writes = append(writes, row)
		}
	}
	if err := r.store.UpsertLendingRevenueUSDDays(ctx, writes); err != nil {
		return nil, err
	}
	st.Written += len(writes)
	obs.LendingRevenueRollupRowsValuedTotal.Add(float64(len(writes)))
	return finished, nil
}

// rollupRun is one lake read of a pool's consecutive queued days: its stored rows and its hour rows by reserve-day.
type rollupRun struct {
	pool  string
	have  map[reserveDay]timescale.LendingRevenueUSDDay
	hours map[reserveDay][]clickhouse.LendingRevenueBucket
}

// valueDay values one queued reserve-day: write is true for a row to store, final when the day reached a final outcome.
// A degraded or unrepresentable day is never written; a refused one is written unpriced, its legs NULL.
func (r *LendingRevenueRollup) valueDay(ctx context.Context, run *rollupRun, t rollupTask, info clickhouse.LendingInterestDay, walks map[reserveDay]reserveUSDRead, st *LendingRevenueRollupStats) (row timescale.LendingRevenueUSDDay, write, final bool) {
	st.PoolDays++
	k := reserveDay{run.pool, info.Asset, info.Day}
	old, exists := run.have[k]
	if t.changeOnly && exists && rollupRowCurrent(old, info) {
		st.Skipped++
		return row, false, true
	}
	d := valueRevenueDay(info.Day, run.hours[k], func() reserveUSDRead {
		wk := reserveDay{asset: info.Asset, day: info.Day}
		w, ok := walks[wk]
		if !ok {
			w = r.walk(ctx, info.Asset, info.Day)
			walks[wk] = w
		}
		return w
	})
	switch d.outcome {
	case revenueDayValued:
	case revenueDayRefused:
		st.Refused++
	case revenueDayDegraded:
		st.Degraded++
		return row, false, false
	}
	sums := clickhouse.NewLendingRevenueSums()
	for _, h := range run.hours[k] {
		sums.Add(h.Sums)
	}
	row, ok := revenueDayRow(run.pool, info.Asset, info, sums, d)
	if !ok {
		st.Degraded++
		r.logger.Error("lending revenue rollup: a leg is not a finite decimal", "pool", run.pool, "asset", info.Asset, "day", info.Day)
		return row, false, false
	}
	switch {
	case exists && revenueDayRowsEqual(old, row):
		st.Unchanged++
		return row, false, true
	case exists && rollupSweepMismatch(t, old, row):
		st.Mismatches++
		obs.LendingRevenueRollupSweepMismatchesTotal.Inc()
		r.logger.Warn("lending revenue rollup: sweep mismatch, rewriting", "pool", run.pool, "asset", info.Asset, "day", info.Day.Format(time.DateOnly))
	}
	return row, true, true
}

// rollupSweepMismatch reports a difference nothing explains: a day only the sweep queued, valued from the same derive at
// the same revisions, neither row a verdict refusal, stating other USD.
func rollupSweepMismatch(t rollupTask, old, row timescale.LendingRevenueUSDDay) bool {
	return t.sweep && !t.explained && old.InterestDerivedAt.Equal(row.InterestDerivedAt) &&
		old.ValuationRev == row.ValuationRev && old.InterestDeriveRev == row.InterestDeriveRev &&
		!revenueDayRefusedRow(old) && !revenueDayRefusedRow(row) && !revenueDayValuesEqual(old, row)
}

// rollupRowCurrent reports whether old was valued from info's derive or a newer one, at the current revisions.
func rollupRowCurrent(old timescale.LendingRevenueUSDDay, info clickhouse.LendingInterestDay) bool {
	return !old.InterestDerivedAt.Before(info.DerivedAt) && old.ValuationRev == lendingRevenueValuationRev &&
		old.InterestDeriveRev == int16(lendinginterest.DeriveRev) //nolint:gosec // a small revision number
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
