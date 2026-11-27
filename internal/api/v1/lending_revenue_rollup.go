package v1

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

var lendingRevenueRollupBudget = rollupBudget{
	cycle: lendingRevenueRollupCycle, cycleTimeout: lendingRevenueRollupCycleTimeout, releaseTimeout: lendingRevenueRollupReleaseTimeout,
	poolDays: lendingRevenueRollupPoolDays, forcedDays: lendingRevenueRollupForcedDays, trailingDays: lendingRevenueRollupTrailingDays,
	fillChunk: lendingRevenueRollupFillChunk, rewalkDays: lendingRevenueRollupRewalkDays, sweepCycles: lendingRevenueRollupSweepCycles,
	maxRun: lendingRevenueRollupMaxRun,
}

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

	cursor clickhouse.LendingInterestDay // change detection's keyset position; zero DerivedAt until bootstrapped
	days   rollupState
	retry  map[poolDay]bool // days whose last valuation reached no final outcome, kept until one does
}

// NewLendingRevenueRollup builds the rollup over s's lake reader and valuer.
func (s *Server) NewLendingRevenueRollup(store LendingRevenueRollupStore, logger *slog.Logger) *LendingRevenueRollup {
	lake, _ := s.Explorer.(lendingRevenueRollupLake)
	walk := func(ctx context.Context, asset string, day time.Time) reserveUSDRead {
		return s.revenueDayRead(ctx, asset, day, lendingRevenueDayWalkBudget)
	}
	return &LendingRevenueRollup{store: store, lake: lake, walk: walk, logger: logger, now: time.Now, retry: map[poolDay]bool{}}
}

// rollup is the shared day-rollup cycle over the revenue reads, valuer and tables.
func (r *LendingRevenueRollup) rollup() *dayRollup {
	return &dayRollup{
		src: dayRollupSource{
			name: "lending revenue rollup", lockName: timescale.LendingRevenueRollupLockName, budget: lendingRevenueRollupBudget,
			ready: r.ready, lock: r.store.HoldInstanceLock, spans: r.spans, bootstrap: r.bootstrap, changes: r.planChanges,
			stale: r.planStale, exclusionsChangedSince: r.store.SourceExclusionWindowsChangedSince, walk: r.walk,
			valueRun: r.executeRun, storeCoverage: r.storeCoverage, started: lendingRevenueRollupStarted,
			succeeded: lendingRevenueRollupSucceeded, mismatched: obs.LendingRevenueRollupSweepMismatchesTotal.Inc,
		},
		logger: r.logger, now: r.now, state: &r.days, retry: r.retry,
	}
}

// Run cycles until ctx ends; a failed cycle is logged and the next one retries.
func (r *LendingRevenueRollup) Run(ctx context.Context) { r.rollup().run(ctx) }

// Cycle is one pass under the lock: the sweep's share, retries, changed derives, stale revisions, changed exclusions, the
// trailing days, then the self-fill, each within its share of [lendingRevenueRollupPoolDays]. [Run] bounds it.
func (r *LendingRevenueRollup) Cycle(ctx context.Context) (LendingRevenueRollupStats, error) {
	return r.rollup().cycle(ctx)
}

func (r *LendingRevenueRollup) ready() error {
	if r.lake == nil {
		return errors.New("lending revenue rollup: the explorer reader has no lending interest reads")
	}
	return nil
}

func lendingRevenueRollupStarted(at time.Time) {
	obs.LendingRevenueRollupStartedUnix.Set(float64(at.Unix()))
}

func lendingRevenueRollupSucceeded(at time.Time, st LendingRevenueRollupStats) {
	obs.LendingRevenueRollupLastSuccessUnix.Set(float64(at.Unix()))
	obs.LendingRevenueRollupDaysNotValued.WithLabelValues("degraded").Set(float64(st.Degraded))
	obs.LendingRevenueRollupDaysNotValued.WithLabelValues("refused").Set(float64(st.Refused))
}

// revenueRollupSpan clips a pool's days as /revenue clips: from the first day starting at or after the derive floor's
// close, through the last day ending by the coverage's next hour and a pending redo's first suspect hour.
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

// spans reads each pool's coverage through the /revenue read itself, over an empty window so no row is read.
func (r *LendingRevenueRollup) spans(ctx context.Context) (map[string]rollupSpan, map[string]rollupCoverage, error) {
	pools, err := r.lake.LendingInterestPools(ctx)
	if err != nil {
		return nil, nil, err
	}
	stored, err := r.store.LendingRevenueUSDCoverages(ctx)
	if err != nil {
		return nil, nil, err
	}
	covs := make(map[string]rollupCoverage, len(stored))
	for pool, c := range stored {
		covs[pool] = rollupCoverage{floorDay: c.FloorDay, throughDay: c.ThroughDay, current: c.ValuationRev == lendingRevenueValuationRev}
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

// bootstrap starts change detection at the newest stored derive (an empty table is the self-fill's).
func (r *LendingRevenueRollup) bootstrap(ctx context.Context, started time.Time) (bool, error) {
	if !r.cursor.DerivedAt.IsZero() {
		return false, nil
	}
	newest, err := r.store.LendingRevenueUSDNewestDerivedAt(ctx)
	if err != nil {
		return false, err
	}
	if newest.IsZero() {
		newest = started
	}
	r.cursor = clickhouse.LendingInterestDay{DerivedAt: newest}
	return true, nil
}

// planChanges queues the days a derive rewrote, paging by keyset so a derive wider than the budget still moves on, and
// re-reads the slack before the cursor for derives that became visible late. A re-derive that only deletes an hour's
// superseded rows writes no newer derived_at; the daily sweep is what catches it.
func (r *LendingRevenueRollup) planChanges(ctx context.Context, p *rollupPlan) (func(), error) {
	cursor := r.cursor
	late, err := r.lake.LendingInterestChangedDays(ctx, clickhouse.LendingInterestDay{DerivedAt: cursor.DerivedAt.Add(-lendingRevenueRollupChangeSlack)}, lendingRevenueRollupChangeLimit)
	if err != nil {
		return nil, err
	}
	if _, err := r.queueChanges(ctx, p, cursor, late); err != nil {
		return nil, err
	}
	changed, err := r.lake.LendingInterestChangedDays(ctx, cursor, lendingRevenueRollupChangeLimit)
	if err != nil {
		return nil, err
	}
	if cursor, err = r.queueChanges(ctx, p, cursor, changed); err != nil {
		return nil, err
	}
	return func() { r.cursor = cursor }, nil
}

// queueChanges queues the changes the store does not already hold, in order, and returns the last change it got past
// (cursor when none). A stored row valued from that derive or a newer one costs no budget.
func (r *LendingRevenueRollup) queueChanges(ctx context.Context, p *rollupPlan, cursor clickhouse.LendingInterestDay, changes []clickhouse.LendingInterestDay) (clickhouse.LendingInterestDay, error) {
	stored, err := r.storedRows(ctx, changes)
	if err != nil {
		return cursor, err
	}
	day := func(c clickhouse.LendingInterestDay) poolDay { return poolDay{c.Pool, c.Day} }
	held := func(c clickhouse.LendingInterestDay) bool {
		old, ok := stored[reserveDay{c.Pool, c.Asset, c.Day}]
		return ok && rollupRowCurrent(old, c)
	}
	return queueChangePage(p, cursor, changes, day, held), nil
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

// storeCoverage writes pool's coverage through through, so inside coverage a missing row only ever means a day with no
// interest rows.
func (r *LendingRevenueRollup) storeCoverage(ctx context.Context, pool string, sp rollupSpan, through time.Time) error {
	c := timescale.LendingRevenueUSDCoverage{
		Pool: pool, FloorDay: sp.floor, ThroughDay: through, RedoFrom: sp.redo,
		InterestDeriveRev: int16(lendinginterest.DeriveRev), ValuationRev: lendingRevenueValuationRev, //nolint:gosec // a small revision number
	}
	return r.store.UpsertLendingRevenueUSDCoverage(ctx, c)
}

// executeRun values one run: the interest days first, then the rows (so a derive in between reads as newer next cycle),
// then the stored days, then one write, with no transaction across a read. finished holds the run's days whose every
// reserve reached a final outcome: valued, stored with its reason, refused and recorded, or proven to have no interest.
func (r *LendingRevenueRollup) executeRun(ctx context.Context, c *rollupCycle, run []poolDay) (map[time.Time]bool, error) {
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
		t, queued := c.tasks[poolDay{pool, info.Day}]
		if !queued || !inRun(info.Day) {
			continue
		}
		row, write, final := valueRevenueRollupDay(ctx, c, &rr, t, info)
		finished[info.Day] = finished[info.Day] && final
		if write {
			writes = append(writes, row)
		}
	}
	if err := r.store.UpsertLendingRevenueUSDDays(ctx, writes); err != nil {
		return nil, err
	}
	c.st.Written += len(writes)
	obs.LendingRevenueRollupRowsValuedTotal.Add(float64(len(writes)))
	return finished, nil
}

// rollupRun is one lake read of a pool's consecutive queued days: its stored rows and its hour rows by reserve-day.
type rollupRun struct {
	pool  string
	have  map[reserveDay]timescale.LendingRevenueUSDDay
	hours map[reserveDay][]clickhouse.LendingRevenueBucket
}

var revenueRollupRows = rollupRows[timescale.LendingRevenueUSDDay]{equal: revenueDayRowsEqual, unexplained: revenueRowsUnexplained}

// valueRevenueRollupDay values one queued reserve-day through the cycle's decision; a refused day is stored with its
// legs NULL.
func valueRevenueRollupDay(ctx context.Context, c *rollupCycle, run *rollupRun, t rollupTask, info clickhouse.LendingInterestDay) (timescale.LendingRevenueUSDDay, bool, bool) {
	k := reserveDay{run.pool, info.Asset, info.Day}
	old, exists := run.have[k]
	return decideRollupDay(c, revenueRollupRows, rollupDayValue[timescale.LendingRevenueUSDDay]{
		key: k, task: t, old: old, exists: exists, current: exists && rollupRowCurrent(old, info),
		value: func() (timescale.LendingRevenueUSDDay, usdDayOutcome, bool) {
			d := valueRevenueDay(info.Day, run.hours[k], func() reserveUSDRead { return c.walk(ctx, info.Asset, info.Day) })
			if d.outcome == usdDayDegraded {
				return timescale.LendingRevenueUSDDay{}, d.outcome, true
			}
			sums := clickhouse.NewLendingRevenueSums()
			for _, h := range run.hours[k] {
				sums.Add(h.Sums)
			}
			row, ok := revenueDayRow(run.pool, info.Asset, info, sums, d)
			return row, d.outcome, ok
		},
	})
}

// revenueRowsUnexplained reports a difference nothing explains: valued from the same derive at the same revisions,
// neither row a verdict refusal, stating other USD.
func revenueRowsUnexplained(old, row timescale.LendingRevenueUSDDay) bool {
	return old.InterestDerivedAt.Equal(row.InterestDerivedAt) &&
		old.ValuationRev == row.ValuationRev && old.InterestDeriveRev == row.InterestDeriveRev &&
		!revenueDayRefusedRow(old) && !revenueDayRefusedRow(row) && !revenueDayValuesEqual(old, row)
}

// rollupRowCurrent reports whether old was valued from info's derive or a newer one, at the current revisions.
func rollupRowCurrent(old timescale.LendingRevenueUSDDay, info clickhouse.LendingInterestDay) bool {
	return !old.InterestDerivedAt.Before(info.DerivedAt) && old.ValuationRev == lendingRevenueValuationRev &&
		old.InterestDeriveRev == int16(lendinginterest.DeriveRev) //nolint:gosec // a small revision number
}
