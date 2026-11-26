package v1

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	lendingRevenueRollupPoolDays       = 600
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
	LendingInterestChangedDays(ctx context.Context, since time.Time, limit int) ([]clickhouse.LendingInterestDay, error)
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
	s      *Server
	store  LendingRevenueRollupStore
	logger *slog.Logger
	now    func() time.Time

	changedSince, exclusionsSince time.Time
	sweepDay                      time.Time
	retry                         map[poolDay]bool
}

type poolDay struct {
	pool string
	day  time.Time
}

// rollupTask is why a pool-day is valued: a change-only task skips a day whose stored row is already current.
type rollupTask struct {
	changeOnly, sweep, fill bool
}

// NewLendingRevenueRollup builds the rollup over s's lake reader and valuer.
func (s *Server) NewLendingRevenueRollup(store LendingRevenueRollupStore, logger *slog.Logger) *LendingRevenueRollup {
	return &LendingRevenueRollup{s: s, store: store, logger: logger, now: time.Now, retry: map[poolDay]bool{}}
}

// Run cycles until ctx ends; a failed cycle is logged and the next one retries.
func (r *LendingRevenueRollup) Run(ctx context.Context) {
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

// rollupPlan is one cycle's queue of pool-days, in queue order and within [lendingRevenueRollupPoolDays].
type rollupPlan struct {
	spans  map[string]rollupSpan
	covs   map[string]timescale.LendingRevenueUSDCoverage
	tasks  map[poolDay]rollupTask
	order  []poolDay
	filled map[string]time.Time // each pool's last self-filled day: its coverage row's through day
}

// add queues pd, merging with a task already queued for it; false when the budget is spent.
func (p *rollupPlan) add(pd poolDay, t rollupTask) bool {
	sp, ok := p.spans[pd.pool]
	if !ok || !sp.holds(pd.day) {
		return true
	}
	if old, seen := p.tasks[pd]; seen {
		old.changeOnly = old.changeOnly && t.changeOnly
		old.sweep, old.fill = old.sweep || t.sweep, old.fill || t.fill
		p.tasks[pd] = old
		return true
	}
	if len(p.order) >= lendingRevenueRollupPoolDays {
		return false
	}
	p.tasks[pd] = t
	p.order = append(p.order, pd)
	return true
}

// Cycle is one pass under the lock: the sweep's share, retries, changed derives, stale revisions, changed exclusions, the
// trailing days, then the self-fill, all within [lendingRevenueRollupPoolDays]. [Run] bounds it.
func (r *LendingRevenueRollup) Cycle(ctx context.Context) (LendingRevenueRollupStats, error) {
	var st LendingRevenueRollupStats
	lake, ok := r.s.Explorer.(lendingRevenueRollupLake)
	if !ok {
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
	started := r.now().UTC()
	p, err := r.plan(cctx, lake, started)
	if err != nil {
		return st, err
	}
	if err := r.execute(cctx, lake, p.tasks, p.order, &st); err != nil {
		return st, err
	}
	if err := r.storeCoverage(cctx, p); err != nil {
		return st, err
	}
	r.exclusionsSince = started
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

// plan queues the cycle's pool-days in priority order, so the budget cuts the self-fill before anything else.
func (r *LendingRevenueRollup) plan(ctx context.Context, lake lendingRevenueRollupLake, started time.Time) (*rollupPlan, error) {
	spans, covs, err := r.spans(ctx, lake)
	if err != nil {
		return nil, err
	}
	if err := r.bootstrap(ctx, started); err != nil {
		return nil, err
	}
	p := &rollupPlan{spans: spans, covs: covs, tasks: map[poolDay]rollupTask{}, filled: map[string]time.Time{}}
	r.planSweep(p)
	for pd := range r.retry {
		p.add(pd, rollupTask{})
	}
	r.retry = map[poolDay]bool{}
	if err := r.planChanges(ctx, lake, p); err != nil {
		return nil, err
	}
	if err := r.planStale(ctx, p); err != nil {
		return nil, err
	}
	if err := r.planExclusions(ctx, p, started); err != nil {
		return nil, err
	}
	for pool, sp := range spans {
		for i := range lendingRevenueRollupRewalkDays {
			p.add(poolDay{pool, sp.through.Add(-time.Duration(i) * day24)}, rollupTask{})
		}
		p.planFill(pool, sp)
	}
	return p, nil
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

func (r *LendingRevenueRollup) storeCoverage(ctx context.Context, p *rollupPlan) error {
	for pool, sp := range p.spans {
		c := timescale.LendingRevenueUSDCoverage{
			Pool: pool, FloorDay: sp.floor, ThroughDay: p.filled[pool], RedoFrom: sp.redo,
			InterestDeriveRev: int16(lendinginterest.DeriveRev), ValuationRev: lendingRevenueValuationRev, //nolint:gosec // a small revision number
		}
		if err := r.store.UpsertLendingRevenueUSDCoverage(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// spans reads each pool's coverage through the /revenue read itself, over an empty window so no row is read.
func (r *LendingRevenueRollup) spans(ctx context.Context, lake lendingRevenueRollupLake) (map[string]rollupSpan, map[string]timescale.LendingRevenueUSDCoverage, error) {
	pools, err := lake.LendingInterestPools(ctx)
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
		rev, err := lake.LendingPoolRevenue(ctx, pool, "", clickhouse.LendingHistoryQuery{From: at, To: at})
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
	if !r.changedSince.IsZero() {
		return nil
	}
	newest, err := r.store.LendingRevenueUSDNewestDerivedAt(ctx)
	if err != nil {
		return err
	}
	r.changedSince = newest
	if newest.IsZero() {
		r.changedSince = started
	}
	r.exclusionsSince = started.Add(-day24)
	return nil
}

// planChanges queues the days a derive rewrote; the watermark moves only past changes that were queued.
func (r *LendingRevenueRollup) planChanges(ctx context.Context, lake lendingRevenueRollupLake, p *rollupPlan) error {
	changed, err := lake.LendingInterestChangedDays(ctx, r.changedSince.Add(-lendingRevenueRollupChangeSlack), lendingRevenueRollupChangeLimit)
	if err != nil {
		return err
	}
	for _, c := range changed {
		if !p.add(poolDay{c.Pool, c.Day}, rollupTask{changeOnly: true}) {
			return nil
		}
		if c.DerivedAt.After(r.changedSince) {
			r.changedSince = c.DerivedAt
		}
	}
	return nil
}

// planExclusions queues every valued day overlapping a source exclusion created or lifted since the last cycle.
func (r *LendingRevenueRollup) planExclusions(ctx context.Context, p *rollupPlan, started time.Time) error {
	wins, err := r.store.SourceExclusionWindowsChangedSince(ctx, r.exclusionsSince)
	if err != nil {
		return err
	}
	for _, w := range wins {
		until := w[1]
		if until.IsZero() {
			until = started
		}
		if !p.addWindow(w[0].Truncate(day24), until) {
			return nil
		}
	}
	return nil
}

// addWindow queues every stored day of every pool from from to before until; false when the budget is spent.
func (p *rollupPlan) addWindow(from, until time.Time) bool {
	for pool, sp := range p.spans {
		last := sp.through
		if c, ok := p.covs[pool]; ok {
			last = minTime(last, c.ThroughDay)
		}
		for d := maxTime(sp.floor, from); d.Before(until) && !d.After(last); d = d.Add(day24) {
			if !p.add(poolDay{pool, d}, rollupTask{}) {
				return false
			}
		}
	}
	return true
}

// planSweep queues this cycle's share of every stored span, all pools on the same days so one walk serves them all.
func (r *LendingRevenueRollup) planSweep(p *rollupPlan) {
	lo, hi, ok := p.storedBounds()
	if !ok {
		return
	}
	n := int(hi.Sub(lo)/day24) + 1
	k := (n + lendingRevenueRollupSweepCycles - 1) / lendingRevenueRollupSweepCycles
	if r.sweepDay.Before(lo) || r.sweepDay.After(hi) {
		r.sweepDay = lo
	}
	for i := 0; i < k && !r.sweepDay.After(hi); i++ {
		for pool := range p.spans {
			if c, ok := p.covs[pool]; ok && !r.sweepDay.After(c.ThroughDay) {
				p.add(poolDay{pool, r.sweepDay}, rollupTask{sweep: true})
			}
		}
		r.sweepDay = r.sweepDay.Add(day24)
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

// execute values the queued days pool by pool in runs of consecutive days: the interest days first, then the rows (so a
// derive in between reads as newer next cycle), then the stored days, then one write, with no transaction across a read.
func (r *LendingRevenueRollup) execute(ctx context.Context, lake lendingRevenueRollupLake, tasks map[poolDay]rollupTask, order []poolDay, st *LendingRevenueRollupStats) error {
	sort.Slice(order, func(i, j int) bool {
		if order[i].pool != order[j].pool {
			return order[i].pool < order[j].pool
		}
		return order[i].day.Before(order[j].day)
	})
	walks := map[poolDay]reserveUSDRead{} // (asset, day): one walk per cycle, whatever pools hold the asset
	for i := 0; i < len(order); {
		j := i + 1
		for j < len(order) && order[j].pool == order[i].pool && j-i < lendingRevenueRollupMaxRun &&
			order[j].day.Sub(order[i].day) < lendingRevenueRollupMaxRun*day24 {
			j++
		}
		if err := r.executeRun(ctx, lake, tasks, order[i:j], walks, st); err != nil {
			return err
		}
		i = j
	}
	return nil
}

func (r *LendingRevenueRollup) executeRun(ctx context.Context, lake lendingRevenueRollupLake, tasks map[poolDay]rollupTask, run []poolDay, walks map[poolDay]reserveUSDRead, st *LendingRevenueRollupStats) error {
	pool, from, last := run[0].pool, run[0].day, run[len(run)-1].day
	before := last.Add(day24)
	infos, err := lake.LendingInterestPoolDays(ctx, pool, from, before)
	if err != nil {
		return err
	}
	days := int(before.Sub(from) / day24)
	rev, err := lake.LendingPoolRevenue(ctx, pool, "", clickhouse.LendingHistoryQuery{From: from, To: before, Daily: true, MaxPoints: days + 1})
	if err != nil {
		return err
	}
	sp, ok := revenueRollupSpan(rev)
	if !ok {
		return nil
	}
	stored, err := r.store.LendingRevenueUSDDays(ctx, pool, from, last)
	if err != nil {
		return err
	}
	rr := rollupRun{pool: pool, have: map[poolDay]timescale.LendingRevenueUSDDay{}, hours: map[poolDay][]clickhouse.LendingRevenueBucket{}}
	for _, d := range stored {
		rr.have[poolDay{d.Asset, d.Day}] = d
	}
	for _, res := range rev.Reserves {
		for _, h := range res.Hours {
			k := poolDay{res.Asset, h.Start.UTC().Truncate(day24)}
			rr.hours[k] = append(rr.hours[k], h)
		}
	}
	var writes []timescale.LendingRevenueUSDDay
	for _, info := range infos {
		t, queued := tasks[poolDay{pool, info.Day}]
		if !queued || !sp.holds(info.Day) || info.Day.Add(day24).After(rev.Before) {
			continue
		}
		if row, write := r.valueDay(ctx, &rr, t, info, walks, st); write {
			writes = append(writes, row)
		}
	}
	if err := r.store.UpsertLendingRevenueUSDDays(ctx, writes); err != nil {
		return err
	}
	st.Written += len(writes)
	obs.LendingRevenueRollupRowsValuedTotal.Add(float64(len(writes)))
	return nil
}

// rollupRun is one lake read of a pool's consecutive queued days: its stored rows and its hour rows by (asset, day).
type rollupRun struct {
	pool  string
	have  map[poolDay]timescale.LendingRevenueUSDDay
	hours map[poolDay][]clickhouse.LendingRevenueBucket
}

// valueDay values one queued reserve-day; write is true for a row to store. A degraded, refused or unrepresentable day
// is never written.
func (r *LendingRevenueRollup) valueDay(ctx context.Context, run *rollupRun, t rollupTask, info clickhouse.LendingInterestDay, walks map[poolDay]reserveUSDRead, st *LendingRevenueRollupStats) (row timescale.LendingRevenueUSDDay, write bool) {
	st.PoolDays++
	k := poolDay{info.Asset, info.Day}
	old, exists := run.have[k]
	if t.changeOnly && exists && rollupRowCurrent(old, info) {
		st.Skipped++
		return row, false
	}
	d := valueRevenueDay(info.Day, run.hours[k], func() reserveUSDRead {
		w, ok := walks[k]
		if !ok {
			w = r.s.revenueDayRead(ctx, info.Asset, info.Day, lendingRevenueDayWalkBudget)
			walks[k] = w
		}
		return w
	})
	switch d.outcome {
	case revenueDayValued:
	case revenueDayDegraded:
		st.Degraded++
		r.retry[poolDay{run.pool, info.Day}] = true
		return row, false
	case revenueDayRefused:
		st.Refused++
		return row, false
	}
	sums := clickhouse.NewLendingRevenueSums()
	for _, h := range run.hours[k] {
		sums.Add(h.Sums)
	}
	row, ok := revenueDayRow(run.pool, info.Asset, info, sums, d)
	if !ok {
		st.Degraded++
		r.logger.Error("lending revenue rollup: a leg is not a finite decimal", "pool", run.pool, "asset", info.Asset, "day", info.Day)
		return row, false
	}
	switch {
	case exists && revenueDayRowsEqual(old, row):
		st.Unchanged++
		return row, false
	case exists && t.sweep && !revenueDayValuesEqual(old, row):
		st.Mismatches++
		obs.LendingRevenueRollupSweepMismatchesTotal.Inc()
		r.logger.Warn("lending revenue rollup: sweep mismatch, rewriting", "pool", run.pool, "asset", info.Asset, "day", info.Day.Format(time.DateOnly))
	}
	return row, true
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
