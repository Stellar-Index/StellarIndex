package v1

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The emissions rollup's cadence and its per-cycle work budget: every pool-day is one /emissions/history read of that day.
const (
	lendingEmissionsRollupCycle          = 10 * time.Minute
	lendingEmissionsRollupCycleTimeout   = 8 * time.Minute
	lendingEmissionsRollupPoolDays       = 360
	lendingEmissionsRollupForcedDays     = lendingEmissionsRollupPoolDays * 2 / 3
	lendingEmissionsRollupTrailingDays   = lendingEmissionsRollupPoolDays * 5 / 6
	lendingEmissionsRollupFillChunk      = 30
	lendingEmissionsRollupRewalkDays     = 3
	lendingEmissionsRollupSweepCycles    = 144 // one UTC day at the cadence
	lendingEmissionsRollupMaxRun         = 31  // days per pool-instance read
	lendingEmissionsRollupReleaseTimeout = 5 * time.Second
	// lendingEmissionsValuationRev is the stored rows' valuation; a row of another one is re-valued.
	lendingEmissionsValuationRev int16 = 1
)

var lendingEmissionsRollupBudget = rollupBudget{
	cycle: lendingEmissionsRollupCycle, cycleTimeout: lendingEmissionsRollupCycleTimeout, releaseTimeout: lendingEmissionsRollupReleaseTimeout,
	poolDays: lendingEmissionsRollupPoolDays, forcedDays: lendingEmissionsRollupForcedDays, trailingDays: lendingEmissionsRollupTrailingDays,
	fillChunk: lendingEmissionsRollupFillChunk, rewalkDays: lendingEmissionsRollupRewalkDays, sweepCycles: lendingEmissionsRollupSweepCycles,
	maxRun: lendingEmissionsRollupMaxRun,
}

// The stored streams: /emissions/history's four, the allocation as its two shares and the pool claims with their
// single-reserve lines apart. Each is its own layer; no row sums two.
const (
	emisRowSchedule      = "reserve_schedule"
	emisRowPoolClaims    = "pool_claims"
	emisRowReserveClaims = "pool_claims_reserve"
	emisRowAllocBackstop = "backstop_alloc_backstop"
	emisRowAllocPool     = "backstop_alloc_pool"
	emisRowTierClaims    = "backstop_claims"
)

// LendingEmissionsRollupStore is the Postgres side of the emissions rollup; the event read is the server's lending reader.
type LendingEmissionsRollupStore interface {
	HoldInstanceLock(ctx context.Context, name string, onLost func(), logger *slog.Logger) (*timescale.InstanceLock, error)
	LendingEmissionPools(ctx context.Context) (map[string]time.Time, error)
	LendingEmissionsUSDDays(ctx context.Context, pool string, from, through time.Time) ([]timescale.LendingEmissionsUSDDay, error)
	ReplaceLendingEmissionsUSDDays(ctx context.Context, pool string, days []time.Time, rows []timescale.LendingEmissionsUSDDay) error
	LendingEmissionsUSDStaleRevDays(ctx context.Context, valuationRev int16, limit int) (map[string][]time.Time, error)
	LendingEmissionsUSDCoverages(ctx context.Context) (map[string]timescale.LendingEmissionsUSDCoverage, error)
	UpsertLendingEmissionsUSDCoverage(ctx context.Context, c timescale.LendingEmissionsUSDCoverage) error
	SourceExclusionWindowsChangedSince(ctx context.Context, since time.Time) ([][2]time.Time, error)
}

// LendingEmissionsRollup keeps lending_emissions_usd_daily: per pool and UTC day, every stream line /emissions/history
// serves for that day at 1d, from the same read and valuer. Its state is in memory; after a restart the daily sweep
// re-proves everything.
type LendingEmissionsRollup struct {
	srv    *Server
	store  LendingEmissionsRollupStore
	logger *slog.Logger
	now    func() time.Time

	booted bool
	pools  map[string]emisRollupPool // the last span read's pools
	days   rollupState
	retry  map[poolDay]bool
}

// emisRollupPool is a pool's lineage and the streams it does not serve, with their reasons.
type emisRollupPool struct {
	protocol, lineage string
	unavailable       map[string]string
}

// NewLendingEmissionsRollup builds the emissions rollup over s's lending reader, lake and valuer.
func (s *Server) NewLendingEmissionsRollup(store LendingEmissionsRollupStore, logger *slog.Logger) *LendingEmissionsRollup {
	return &LendingEmissionsRollup{srv: s, store: store, logger: logger, now: time.Now, pools: map[string]emisRollupPool{}, retry: map[poolDay]bool{}}
}

func (r *LendingEmissionsRollup) rollup() *dayRollup {
	return &dayRollup{
		src: dayRollupSource{
			name: "lending emissions rollup", lockName: timescale.LendingEmissionsRollupLockName, budget: lendingEmissionsRollupBudget,
			ready: r.ready, lock: r.store.HoldInstanceLock, spans: r.spans, bootstrap: r.bootstrap, changes: r.planChanges,
			stale: r.planStale, exclusionsChangedSince: r.store.SourceExclusionWindowsChangedSince,
			valueRun: r.executeRun, storeCoverage: r.storeCoverage, started: lendingEmissionsRollupStarted,
			succeeded: lendingEmissionsRollupSucceeded, mismatched: obs.LendingEmissionsRollupSweepMismatchesTotal.Inc,
		},
		logger: r.logger, now: r.now, state: &r.days, retry: r.retry,
	}
}

// Run cycles until ctx ends; a failed cycle is logged and the next one retries.
func (r *LendingEmissionsRollup) Run(ctx context.Context) { r.rollup().run(ctx) }

// Cycle is one pass under the lock: the sweep's share, retries, stale revisions, changed exclusions, the trailing days,
// then the self-fill. [Run] bounds it.
func (r *LendingEmissionsRollup) Cycle(ctx context.Context) (LendingRevenueRollupStats, error) {
	return r.rollup().cycle(ctx)
}

func (r *LendingEmissionsRollup) reader() (lendingEmissionsHistoryReader, bool) {
	hr, ok := r.srv.Lending.(lendingEmissionsHistoryReader)
	return hr, ok
}

func (r *LendingEmissionsRollup) ready() error {
	if _, ok := r.reader(); !ok {
		return errors.New("lending emissions rollup: the lending reader has no emissions history read")
	}
	return nil
}

func lendingEmissionsRollupStarted(at time.Time) {
	obs.LendingEmissionsRollupStartedUnix.Set(float64(at.Unix()))
}

func lendingEmissionsRollupSucceeded(at time.Time, st LendingRevenueRollupStats) {
	obs.LendingEmissionsRollupLastSuccessUnix.Set(float64(at.Unix()))
	obs.LendingEmissionsRollupDaysNotValued.WithLabelValues("degraded").Set(float64(st.Degraded))
	obs.LendingEmissionsRollupDaysNotValued.WithLabelValues("refused").Set(float64(st.Refused))
}

// spans are each known pool's whole served days: from its first emission event's day through the last day both its
// writers and the lake have passed. A pool whose tip or writer cannot be placed is left out this cycle.
func (r *LendingEmissionsRollup) spans(ctx context.Context) (map[string]rollupSpan, map[string]rollupCoverage, error) {
	first, err := r.store.LendingEmissionPools(ctx)
	if err != nil {
		return nil, nil, err
	}
	stored, err := r.store.LendingEmissionsUSDCoverages(ctx)
	if err != nil {
		return nil, nil, err
	}
	covs := make(map[string]rollupCoverage, len(stored))
	for pool, c := range stored {
		covs[pool] = rollupCoverage{floorDay: c.FloorDay, throughDay: c.ThroughDay, current: c.ValuationRev == lendingEmissionsValuationRev}
	}
	now := r.now().UTC()
	spans, pools := map[string]rollupSpan{}, map[string]emisRollupPool{}
	for pool, at := range first {
		protocol, lineage, err := r.srv.backstopLineage(ctx, pool)
		if err != nil {
			return nil, nil, fmt.Errorf("lending emissions rollup: lineage of %s: %w", pool, err)
		}
		if lineage == backstopReasonUnknown {
			continue
		}
		info := emisRollupPool{protocol: protocol, lineage: lineage, unavailable: emisUnavailable(protocol, lineage)}
		if sp, ok := r.span(ctx, pool, info, at.UTC().Truncate(day24), now); ok {
			spans[pool], pools[pool] = sp, info
		}
	}
	r.pools = pools
	return spans, covs, nil
}

// span bounds a pool's days as /emissions/history bounds its window: the lake tip and each stream group's writer.
func (r *LendingEmissionsRollup) span(ctx context.Context, pool string, info emisRollupPool, floor, now time.Time) (rollupSpan, bool) {
	isBLNT, isV1 := info.protocol == blnt.SourceName, info.lineage == backstopReasonV1
	view, win, flags := r.srv.emissionsHistoryView(ctx, pool, info.protocol, "1d", clickhouse.LendingHistoryQuery{From: floor, To: now, Daily: true})
	b := r.srv.emissionsWriterBounds(ctx, win, isBLNT, isV1, &flags)
	for _, e := range view.Excluded {
		if e.Reason == emisHistTipUnknown {
			return rollupSpan{}, false
		}
	}
	if flags.Degraded || b.poolReason == emisHistWriterUnread || (!isV1 && b.backstopReason == emisHistWriterUnread) {
		return rollupSpan{}, false
	}
	sp := rollupSpan{floor: floor, through: minTime(b.pool.end, b.backstop.end).UTC().Truncate(day24).Add(-day24)}
	return sp, !sp.through.Before(sp.floor)
}

// emisUnavailable is each stream /emissions/history does not serve for a pool of this lineage, with its reason.
func emisUnavailable(protocol, lineage string) map[string]string {
	var v LendingEmissionsHistoryView
	fillEmissionsHistory(&v, timescale.LendingEmissionsHistory{}, emissionsBounds{}, nil, protocol == blnt.SourceName, lineage == backstopReasonV1)
	out := map[string]string{}
	for name, h := range map[string]lendingEmissionStreamHead{
		emisStreamReserves: v.Reserves.lendingEmissionStreamHead, emisStreamPoolClaims: v.PoolClaims.lendingEmissionStreamHead,
		emisStreamBackstopAlloc: v.BackstopAlloc.lendingEmissionStreamHead, emisStreamBackstopClaims: v.BackstopClaims.lendingEmissionStreamHead,
	} {
		if !h.Available && h.UnavailableReason != nil {
			out[name] = *h.UnavailableReason
		}
	}
	return out
}

// bootstrap reports a first cycle, so exclusions are read from a day back.
func (r *LendingEmissionsRollup) bootstrap(context.Context, time.Time) (bool, error) {
	booted := !r.booted
	r.booted = true
	return booted, nil
}

// planChanges queues nothing: the event tables carry no change cursor. A day enters the span only once both its writers
// and the lake have passed it, and the trailing days are re-read every cycle; a replay rewriting an older day is what the
// daily sweep catches, within one sweep period.
func (r *LendingEmissionsRollup) planChanges(context.Context, *rollupPlan) (func(), error) {
	return func() {}, nil
}

// planStale queues the stored days of another valuation revision.
func (r *LendingEmissionsRollup) planStale(ctx context.Context, p *rollupPlan) error {
	stale, err := r.store.LendingEmissionsUSDStaleRevDays(ctx, lendingEmissionsValuationRev, lendingEmissionsRollupPoolDays)
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

// storeCoverage writes pool's coverage through through, so inside coverage a missing row only ever means a stream line
// with no tokens that day.
func (r *LendingEmissionsRollup) storeCoverage(ctx context.Context, pool string, sp rollupSpan, through time.Time) error {
	info := r.pools[pool]
	return r.store.UpsertLendingEmissionsUSDCoverage(ctx, timescale.LendingEmissionsUSDCoverage{
		Pool: pool, Protocol: info.protocol, FloorDay: sp.floor, ThroughDay: through, Unavailable: info.unavailable,
		ValuationRev: lendingEmissionsValuationRev,
	})
}

// executeRun values each queued day of one pool as its own /emissions/history?interval=1d read, then replaces the days
// whose rows changed in one write. finished holds the days that reached a final outcome.
func (r *LendingEmissionsRollup) executeRun(ctx context.Context, c *rollupCycle, run []poolDay) (map[time.Time]bool, error) {
	pool, from, last := run[0].pool, run[0].day, run[len(run)-1].day
	info, ok := r.pools[pool]
	hr, hasReader := r.reader()
	if !ok || !hasReader {
		return map[time.Time]bool{}, nil
	}
	st, err := r.srv.emissionsHistoryStorage(ctx, pool, info.protocol == blnt.SourceName, info.lineage == backstopReasonV1)
	if err != nil {
		return nil, err
	}
	stored, err := r.store.LendingEmissionsUSDDays(ctx, pool, from, last)
	if err != nil {
		return nil, err
	}
	have := map[time.Time][]timescale.LendingEmissionsUSDDay{}
	for _, d := range stored {
		have[d.Day] = append(have[d.Day], d)
	}
	finished := map[time.Time]bool{}
	var days []time.Time
	var writes []timescale.LendingEmissionsUSDDay
	for _, pd := range run {
		rows, write, final, err := r.valueDay(ctx, c, hr, info, st, pd, have[pd.day])
		if err != nil {
			return nil, err
		}
		finished[pd.day] = final
		if write {
			days, writes = append(days, pd.day), append(writes, rows...)
		}
	}
	if err := r.store.ReplaceLendingEmissionsUSDDays(ctx, pool, days, writes); err != nil {
		return nil, err
	}
	c.st.Written += len(writes)
	obs.LendingEmissionsRollupRowsValuedTotal.Add(float64(len(writes)))
	return finished, nil
}

var emisRollupRows = rollupRows[[]timescale.LendingEmissionsUSDDay]{equal: emisDayRowsEqual, unexplained: emisDayRowsUnexplained}

// valueDay values one queued pool-day through the cycle's decision. A degraded read, or a day the read does not serve
// whole, is never written and is retried.
func (r *LendingEmissionsRollup) valueDay(ctx context.Context, c *rollupCycle, hr lendingEmissionsHistoryReader, info emisRollupPool, st emisStorage, pd poolDay, old []timescale.LendingEmissionsUSDDay) ([]timescale.LendingEmissionsUSDDay, bool, bool, error) {
	var readErr error
	rows, write, final := decideRollupDay(c, emisRollupRows, rollupDayValue[[]timescale.LendingEmissionsUSDDay]{
		key: reserveDay{pool: pd.pool, day: pd.day}, task: c.tasks[pd], old: old, exists: len(old) > 0,
		value: func() ([]timescale.LendingEmissionsUSDDay, usdDayOutcome, bool) {
			// The request's own budget, so the walk is cut where the endpoint's would be.
			rctx, cancel := context.WithTimeout(ctx, maxHandlerBudget)
			defer cancel()
			q := clickhouse.LendingHistoryQuery{From: pd.day, To: pd.day.Add(day24), Daily: true}
			view, _, cut, err := r.srv.emissionsHistory(rctx, hr, pd.pool, info.protocol, info.lineage, "1d", q, st)
			if err != nil {
				readErr = err
				return nil, usdDayDegraded, true
			}
			if cut || time.Time(view.ServedThrough).Before(q.To) {
				return nil, usdDayDegraded, true
			}
			rows, ok, offDay := emissionsDayRows(&view, pd.day)
			if offDay != nil {
				readErr = fmt.Errorf("lending emissions rollup: %s day %s: the read serves a point at %s, which no row reaches",
					pd.pool, pd.day.Format(time.DateOnly), offDay.Format(time.RFC3339))
				return nil, usdDayDegraded, true
			}
			if len(rows) == 0 {
				c.st.Empty++
			}
			return rows, emisDayOutcome(rows), ok
		},
	})
	if readErr != nil {
		return nil, false, false, readErr
	}
	return rows, write && (len(old) > 0 || len(rows) > 0), final, nil
}

// emisDayOutcome is refused when a read-time verdict or the token identity gate left a row unpriced.
func emisDayOutcome(rows []timescale.LendingEmissionsUSDDay) usdDayOutcome {
	for _, d := range rows {
		if emisRowRefused(d) {
			return usdDayRefused
		}
	}
	return usdDayValued
}

func emisRowRefused(d timescale.LendingEmissionsUSDDay) bool {
	if d.USDReason == nil {
		return false
	}
	switch *d.USDReason {
	case exclusionPriceWithheld, emissionUSDTokenUnverified, emissionUSDTokenUnread:
		return true
	}
	return false
}

// emissionsDayRows is every stream line the view serves for day, one row each, exact as served before rounding; false
// when a USD figure is not a finite decimal. offDay is a point the day's read serves at another time: no row would hold
// it, so the day cannot be called empty.
func emissionsDayRows(v *LendingEmissionsHistoryView, day time.Time) (_ []timescale.LendingEmissionsUSDDay, ok bool, offDay *time.Time) {
	b := emisRowBuilder{v: v, day: day, ok: true}
	if v.Reserves.Available {
		b.schedule()
	}
	b.poolClaims()
	if v.BackstopAlloc.Available {
		b.alloc()
	}
	if bc := &v.BackstopClaims; bc.Available {
		for _, tier := range bc.Tiers {
			for _, p := range tier.Points {
				if b.at(p.T) {
					b.out = append(b.out, b.row(emisRowTierClaims, tier.Tier, bc.LowerBound, []string{emisStreamBackstopClaims}, p.BLNTAmount, &p.Claims, p.USD))
				}
			}
		}
	}
	return b.out, b.ok, b.offDay
}

func (b *emisRowBuilder) at(t WireTime) bool {
	if time.Time(t).Equal(b.day) {
		return true
	}
	if b.offDay == nil {
		off := time.Time(t).UTC()
		b.offDay = &off
	}
	return false
}

func (b *emisRowBuilder) schedule() {
	rs := &b.v.Reserves
	for _, line := range rs.Streams {
		approx, reason := rs.Approximate, rs.ApproximateReason
		if line.Approximate {
			approx, reason = true, line.ApproximateReason
		}
		for _, p := range line.Points {
			if !b.at(p.T) {
				continue
			}
			r := b.row(emisRowSchedule, strconv.FormatUint(uint64(line.TokenID), 10), rs.LowerBound, []string{emisStreamReserves}, p.Tokens, nil, p.USD)
			r.Asset, r.Side = line.Asset, nonEmptyPtr(line.Side)
			if approx {
				r.Approximate, r.ApproximateReason = true, reason
			}
			b.out = append(b.out, r)
		}
	}
}

func (b *emisRowBuilder) poolClaims() {
	pc := &b.v.PoolClaims
	for _, p := range pc.Points {
		if b.at(p.T) {
			b.out = append(b.out, b.row(emisRowPoolClaims, "", pc.LowerBound, []string{emisStreamPoolClaims}, p.Amount, &p.Claims, p.USD))
		}
	}
	for _, line := range pc.ByReserve.Reserves {
		for _, p := range line.Points {
			if !b.at(p.T) {
				continue
			}
			r := b.row(emisRowReserveClaims, strconv.FormatUint(uint64(line.TokenID), 10), pc.ByReserve.LowerBound,
				[]string{emisStreamPoolClaims, emisStreamClaimsByReserve}, p.Amount, &p.Claims, p.USD)
			r.Asset, r.Side = line.Asset, nonEmptyPtr(line.Side)
			b.out = append(b.out, r)
		}
	}
}

// alloc stores the allocation's two shares as two lines, each with the shares' shared coverage.
func (b *emisRowBuilder) alloc() {
	ba := &b.v.BackstopAlloc
	for _, p := range ba.Points {
		if !b.at(p.T) {
			continue
		}
		for i, leg := range []struct{ stream, amount string }{{emisRowAllocBackstop, p.BackstopEmissions}, {emisRowAllocPool, p.PoolEmissions}} {
			usd := LendingEmissionUSD{lendingEmissionUSDCoverage: p.USD.lendingEmissionUSDCoverage, exact: p.USD.exact[i]}
			usd.AmountUSD = []*string{p.USD.BackstopEmissionsUSD, p.USD.PoolEmissionsUSD}[i]
			b.out = append(b.out, b.row(leg.stream, "", ba.LowerBound, []string{emisStreamBackstopAlloc}, leg.amount, &p.Events, usd))
		}
	}
}

type emisRowBuilder struct {
	v      *LendingEmissionsHistoryView
	day    time.Time
	out    []timescale.LendingEmissionsUSDDay
	ok     bool
	offDay *time.Time
}

// row is one stream line's day: its tokens, its served USD before rounding with the hours behind it, and the lower bound
// and exclusions of its stream (streams names it beside the window).
func (b *emisRowBuilder) row(stream, line string, streamLB bool, streams []string, amount string, events *int64, u LendingEmissionUSD) timescale.LendingEmissionsUSDDay {
	r := timescale.LendingEmissionsUSDDay{
		Pool: b.v.Pool, Day: b.day, Stream: stream, Line: line, Protocol: b.v.Protocol, EmissionToken: b.v.EmissionToken,
		EmissionTokenDecimals: b.v.EmissionTokenDecimals, Amount: amount, HoursValued: u.PricedHours,
		HoursUnvalued: u.UnpricedHours, HoursCarried: u.PriceCarriedHours, PriceBasis: nonEmptyPtr(u.PriceBasis),
		BasisHours: map[string]int{}, LowerBound: u.LowerBound, StreamLowerBound: streamLB, Excluded: []string{},
		USDReason: u.USDReason, ValuationRev: lendingEmissionsValuationRev,
	}
	if events != nil {
		n := *events
		r.Events = &n
	}
	for k, n := range u.PriceBasisHours {
		r.BasisHours[k] = n
	}
	for _, e := range b.v.Excluded {
		if (e.Stream == emisStreamWindow || slices.Contains(streams, e.Stream)) && !slices.Contains(r.Excluded, e.Reason) {
			r.Excluded = append(r.Excluded, e.Reason)
		}
	}
	slices.Sort(r.Excluded)
	if u.AmountUSD != nil {
		s, ok := "", u.exact != nil
		if ok {
			s, ok = ratExactDecimal(u.exact)
		}
		b.ok = b.ok && ok
		r.AmountUSD = &s
	}
	return r
}

// emissionUSDFromRow is a stored row as /emissions/history serves its day's usd: the exact USD rounded once.
func emissionUSDFromRow(d timescale.LendingEmissionsUSDDay) (LendingEmissionUSD, error) {
	u := LendingEmissionUSD{lendingEmissionUSDCoverage: lendingEmissionUSDCoverage{
		PricedHours: d.HoursValued, UnpricedHours: d.HoursUnvalued, PriceCarriedHours: d.HoursCarried,
		LowerBound: d.LowerBound, USDReason: d.USDReason,
	}}
	if d.PriceBasis != nil {
		u.PriceBasis = *d.PriceBasis
	}
	if len(d.BasisHours) > 0 {
		u.PriceBasisHours = d.BasisHours
	}
	if d.AmountUSD != nil {
		x, ok := new(big.Rat).SetString(*d.AmountUSD)
		if !ok {
			return u, fmt.Errorf("lending emissions rollup: stored usd %q is not a decimal", *d.AmountUSD)
		}
		s := ratString(x, 2)
		u.AmountUSD, u.exact = &s, x
	}
	u.Approximate = d.Approximate && u.AmountUSD != nil
	return u, nil
}

func emisRowKey(d timescale.LendingEmissionsUSDDay) string { return d.Stream + "\x00" + d.Line }

func emisRowsByKey(rows []timescale.LendingEmissionsUSDDay) map[string]timescale.LendingEmissionsUSDDay {
	out := make(map[string]timescale.LendingEmissionsUSDDay, len(rows))
	for _, d := range rows {
		d.ValuedAt = time.Time{}
		out[emisRowKey(d)] = d
	}
	return out
}

// emisDayRowsEqual: the same lines with every stored fact equal.
func emisDayRowsEqual(old, rows []timescale.LendingEmissionsUSDDay) bool {
	return len(old) == len(rows) && reflect.DeepEqual(emisRowsByKey(old), emisRowsByKey(rows))
}

// emisDayRowsUnexplained: the same lines with the same tokens at the same revision, none refused on either side, and
// other USD. Different tokens are a rewritten source; a refusal is a verdict.
func emisDayRowsUnexplained(old, rows []timescale.LendingEmissionsUSDDay) bool {
	a, b := emisRowsByKey(old), emisRowsByKey(rows)
	if len(a) != len(b) {
		return false
	}
	differs := false
	for k, o := range a {
		n, ok := b[k]
		if !ok || o.Amount != n.Amount || o.ValuationRev != n.ValuationRev || emisRowRefused(o) || emisRowRefused(n) {
			return false
		}
		differs = differs || !reflect.DeepEqual(o.AmountUSD, n.AmountUSD)
	}
	return differs
}
