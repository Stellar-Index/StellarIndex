package v1

import (
	"context"
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// lendingRevenueUSDWindowReader is the Postgres side of /v1/lending/protocols' revenue block.
type lendingRevenueUSDWindowReader interface {
	LendingRevenueUSDCoverages(ctx context.Context) (map[string]timescale.LendingRevenueUSDCoverage, error)
	LendingRevenueUSDBucketSums(ctx context.Context, spans []timescale.LendingRevenueUSDSpan, valuationRev, deriveRev int16, withheld string) ([]timescale.LendingRevenueUSDBucketSum, error)
}

var _ lendingRevenueUSDWindowReader = (*timescale.Store)(nil)

// The revenue block's exclusion reasons beside redo_pending, price_missing, amounts_excluded, withheld, price_read_failed
// and pool_read_failed, which mean here what they mean on /revenue and above.
const (
	protocolRevenueNotValued      = "not_valued"           // the pool has no valued day yet
	protocolRevenueDaysNotValued  = "days_not_valued"      // window days outside the pool's valued days
	protocolRevenueStale          = "valuation_stale"      // days stored at another valuation or derive revision, left out
	protocolRevenueReadFailed     = "revenue_read_failed"  // the stored days could not be read
	protocolRevenueVersionUnknown = "pool_version_unknown" // a Blend pool neither V1 nor V2, left out so versions add up
	protocolRevenueAllExcluded    = "all_excluded"         // usd_reason: every amount in the window was left out
	protocolRevenueTimedOut       = "revenue_timed_out"    // the block's reads ran past protocolRevenueBudget
)

// protocolRevenueBudget bounds the revenue block's reads inside the protocols build, so a slow price or lake read
// nulls the block rather than holding up the rest of the response.
const protocolRevenueBudget = 5 * time.Second

// protocolRevenueWindows are the windows in wire order: days back from through_day, 0 meaning from the earliest floor.
var protocolRevenueWindows = [...]int{1, 7, 30, 0}

// LendingProtocolRevenue is a protocol's interest revenue in USD over complete UTC days ending at through_day, summed from
// the stored day-by-day valuation (lending_revenue_usd_daily).
type LendingProtocolRevenue struct {
	// FeeRateInferred: protocol_fee_usd rests on a fee rate inferred from chain data (BLNT).
	FeeRateInferred bool `json:"fee_rate_inferred"`
	// Degraded: a read behind the block failed or timed out; excluded names what it left out.
	Degraded bool                         `json:"degraded"`
	Day      LendingProtocolRevenueWindow `json:"1d"`
	Week     LendingProtocolRevenueWindow `json:"7d"`
	Month    LendingProtocolRevenueWindow `json:"30d"`
	AllTime  LendingProtocolRevenueWindow `json:"all_time"`
}

// LendingProtocolRevenueWindow is one window's sums, [from_day, through_day] inclusive; both null when no pool is valued.
type LendingProtocolRevenueWindow struct {
	FromDay    *string `json:"from_day"`
	ThroughDay *string `json:"through_day"`
	LendingProtocolRevenueAmounts
	Excluded []LendingProtocolRevenueExclusion `json:"excluded"`
	// Versions splits a Blend window by pool version, each rounded on its own: v1+v2 may miss the total by a cent. Null for BLNT.
	Versions *LendingProtocolRevenueVersions `json:"versions"`
}

// LendingProtocolRevenueVersions is a Blend window by pool version.
type LendingProtocolRevenueVersions struct {
	V1 LendingProtocolRevenueAmounts `json:"v1"`
	V2 LendingProtocolRevenueAmounts `json:"v2"`
}

// LendingProtocolRevenueAmounts are exact sums rounded once to cents, null together with usd_reason when nothing is valued.
type LendingProtocolRevenueAmounts struct {
	InterestAccruedUSD *string `json:"interest_accrued_usd"`
	SupplierShareUSD   *string `json:"supplier_share_usd"`
	BackstopTakeUSD    *string `json:"backstop_take_usd"`
	ProtocolFeeUSD     *string `json:"protocol_fee_usd"`
	BackstopDrawnUSD   *string `json:"backstop_drawn_usd"`
	// BackstopDrawnLowerBound is always true: interest auction draws are a lower bound on every lending route.
	BackstopDrawnLowerBound bool    `json:"backstop_drawn_lower_bound"`
	DefaultLossUSD          *string `json:"default_loss_usd"`
	LowerBound              bool    `json:"lower_bound"`
	USDReason               *string `json:"usd_reason"`
}

// LendingProtocolRevenueExclusion names what a window leaves out; Count is days, hours or versions by reason.
type LendingProtocolRevenueExclusion struct {
	Pool   *string `json:"pool"` // null only for pool_list_read_failed
	Asset  *string `json:"asset"`
	Reason string  `json:"reason"`
	Count  *int64  `json:"count"`
}

// revenuePool is one listed pool as the block sees it: its version ("v1", "v2", "" for BLNT) or why it is left out.
type revenuePool struct {
	pool, version, skip string
}

// protocolRevenueInput is everything one protocol's block is folded from.
type protocolRevenueInput struct {
	fee, versioned bool
	listFailed     bool // the protocol's pool list could not be read
	pools          []revenuePool
	covs           map[string]timescale.LendingRevenueUSDCoverage
	sums           []timescale.LendingRevenueUSDBucketSum
	verdicts       map[string]string // asset → why a read-time verdict refuses it now
}

// revenueCovered is a pool's coverage when it holds at least one valued day at the current revisions: coverage another
// valuer or derive wrote cannot vouch that a missing row is a day with nothing to value.
func revenueCovered(covs map[string]timescale.LendingRevenueUSDCoverage, p revenuePool) (timescale.LendingRevenueUSDCoverage, bool) {
	c, ok := covs[p.pool]
	current := c.ValuationRev == lendingRevenueValuationRev && c.InterestDeriveRev == int16(lendinginterest.DeriveRev) //nolint:gosec // a small revision number
	return c, ok && current && p.skip == "" && !c.ThroughDay.Before(c.FloorDay)
}

// protocolRevenueSpan is the block's [coveredSpan] over its valued pools.
func protocolRevenueSpan(pools []revenuePool, covs map[string]timescale.LendingRevenueUSDCoverage) (floor, through time.Time, ok bool) {
	var cs []coveredDays
	for _, p := range pools {
		if c, covered := revenueCovered(covs, p); covered {
			cs = append(cs, coveredDays{c.FloorDay, c.ThroughDay})
		}
	}
	return coveredSpan(cs)
}

// revenueAcc is one set of exact USD sums: covered counts pools valued inside the window, summed the reserves added.
type revenueAcc struct {
	legs            [revenueUSDLegCount]*big.Rat
	covered, summed int
	lower, excluded bool
}

func (a *revenueAcc) add(legs [revenueUSDLegCount]*big.Rat) {
	for i, x := range legs {
		if a.legs[i] == nil {
			a.legs[i] = new(big.Rat)
		}
		if x != nil {
			a.legs[i].Add(a.legs[i], x)
		}
	}
	a.summed++
}

func (a *revenueAcc) wire() LendingProtocolRevenueAmounts {
	out := LendingProtocolRevenueAmounts{BackstopDrawnLowerBound: true, LowerBound: a.lower}
	switch {
	case a.covered == 0:
		out.USDReason = strRef(protocolRevenueNotValued)
		return out
	case a.summed == 0 && a.excluded:
		out.USDReason = strRef(protocolRevenueAllExcluded)
		return out
	}
	leg := func(i int) *string {
		x := a.legs[i]
		if x == nil {
			x = new(big.Rat)
		}
		return strRef(ratString(x, 2))
	}
	out.InterestAccruedUSD, out.SupplierShareUSD, out.BackstopTakeUSD = leg(0), leg(1), leg(3)
	out.ProtocolFeeUSD, out.BackstopDrawnUSD, out.DefaultLossUSD = leg(4), leg(5), leg(6)
	return out
}

// revenueReserveWin is one (pool, asset)'s stored groups inside a window.
type revenueReserveWin struct {
	legs                             [revenueUSDLegCount]*big.Rat
	priced                           bool
	days, staleDays, hours, withheld int64
	excluded, unreadable             int64
}

// addGroup folds one stored group in; a leg that is not a decimal leaves the whole group out as unreadable.
func (r *revenueReserveWin) addGroup(g timescale.LendingRevenueUSDBucketSum) {
	if !g.Current {
		r.staleDays += g.Days
		return
	}
	var legs [revenueUSDLegCount]*big.Rat
	for i, l := range g.Legs {
		if l == nil {
			continue
		}
		x, ok := new(big.Rat).SetString(*l)
		if !ok {
			r.unreadable += g.Days
			return
		}
		legs[i] = x
	}
	r.days, r.hours, r.withheld, r.excluded = r.days+g.Days, r.hours+g.HoursUnvalued, r.withheld+g.WithheldDays, r.excluded+g.ExcludedCount
	for i, x := range legs {
		if x == nil {
			continue
		}
		if r.legs[i] == nil {
			r.legs[i] = new(big.Rat)
		}
		r.legs[i].Add(r.legs[i], x)
		r.priced = true
	}
}

// revenueWindowFold is one window being folded: the window, its total and the per-version sums.
type revenueWindowFold struct {
	w       LendingProtocolRevenueWindow
	total   revenueAcc
	accs    map[string]*revenueAcc
	version map[string]string
}

func (f *revenueWindowFold) exclude(pool, asset, reason string, count *int64) {
	f.w.Excluded = append(f.w.Excluded, LendingProtocolRevenueExclusion{Pool: nonEmptyPtr(pool), Asset: nonEmptyPtr(asset), Reason: reason, Count: count})
	f.each(pool, func(a *revenueAcc) { a.lower = true })
}

// each applies fn to the total and to pool's version.
func (f *revenueWindowFold) each(pool string, fn func(a *revenueAcc)) {
	fn(&f.total)
	if a := f.accs[f.version[pool]]; a != nil {
		fn(a)
	}
}

// pools names every pool left out of, or short in, [from, through] and counts the pools valued inside it.
func (f *revenueWindowFold) pools(in protocolRevenueInput, from, through time.Time) {
	if in.listFailed {
		f.exclude("", "", exclusionPoolListFailed, nil)
	}
	for _, p := range in.pools {
		f.version[p.pool] = p.version
		c, covered := revenueCovered(in.covs, p)
		switch {
		case p.skip != "":
			f.exclude(p.pool, "", p.skip, nil)
			continue
		case !covered:
			f.exclude(p.pool, "", protocolRevenueNotValued, nil)
			continue
		}
		overlap, missing := coveredDays{c.FloorDay, c.ThroughDay}.overlap(from, through)
		if missing > 0 {
			f.exclude(p.pool, "", protocolRevenueDaysNotValued, &missing)
		}
		if !c.RedoFrom.IsZero() && !c.RedoFrom.After(through) {
			f.exclude(p.pool, "", revenueReasonRedoPending, nil)
		}
		if overlap > 0 {
			f.each(p.pool, func(a *revenueAcc) { a.covered++ })
		}
	}
}

// reserve sums one reserve's window, naming what it leaves out; a reserve refused now adds nothing.
func (f *revenueWindowFold) reserve(k revenueReserveKey, verdict string) {
	r, pool, asset := k.win, k.pool, k.asset
	left := func(reason string, n int64) {
		if n > 0 {
			f.exclude(pool, asset, reason, &n)
			f.each(pool, func(a *revenueAcc) { a.excluded = true })
		}
	}
	if verdict != "" {
		left(verdict, r.days+r.staleDays+r.unreadable)
		return
	}
	left(protocolRevenueStale, r.staleDays)
	left(protocolRevenueReadFailed, r.unreadable)
	left(exclusionPriceWithheld, r.withheld)
	left(revenueUSDPriceMissing, r.hours)
	if r.excluded > 0 {
		f.exclude(pool, asset, revenueUSDAmountsExcluded, &r.excluded)
	}
	if r.priced {
		f.each(pool, func(a *revenueAcc) { a.add(r.legs) })
	}
}

func foldProtocolRevenue(in protocolRevenueInput) LendingProtocolRevenue {
	floor, through, ok := protocolRevenueSpan(in.pools, in.covs)
	out := LendingProtocolRevenue{FeeRateInferred: in.fee}
	for i, w := range [...]*LendingProtocolRevenueWindow{&out.Day, &out.Week, &out.Month, &out.AllTime} {
		*w = foldRevenueWindow(in, protocolWindowFrom(i, floor, through), through, ok, i)
	}
	for _, e := range out.AllTime.Excluded { // all_time holds every pool and reserve the others do
		switch e.Reason {
		case exclusionPriceReadFailed, protocolRevenueReadFailed, protocolRevenueTimedOut, exclusionPoolListFailed:
			out.Degraded = true
		}
	}
	return out
}

// foldRevenueWindow sums the window [from, through] holding buckets 0..maxBucket.
func foldRevenueWindow(in protocolRevenueInput, from, through time.Time, ok bool, maxBucket int) LendingProtocolRevenueWindow {
	f := &revenueWindowFold{
		w:    LendingProtocolRevenueWindow{Excluded: []LendingProtocolRevenueExclusion{}},
		accs: map[string]*revenueAcc{"v1": {}, "v2": {}}, version: map[string]string{},
	}
	if ok {
		f.w.FromDay, f.w.ThroughDay = strRef(from.Format(time.DateOnly)), strRef(through.Format(time.DateOnly))
	}
	f.pools(in, from, through)
	for _, k := range revenueReserveKeys(in, maxBucket) {
		f.reserve(k, in.verdicts[k.asset])
	}
	f.w.LendingProtocolRevenueAmounts = f.total.wire()
	if in.versioned {
		f.w.Versions = &LendingProtocolRevenueVersions{V1: f.accs["v1"].wire(), V2: f.accs["v2"].wire()}
	}
	return f.w
}

type revenueReserveKey struct {
	pool, asset string
	win         *revenueReserveWin
}

// revenueReserveKeys folds the groups of buckets 0..maxBucket per (pool, asset) of an unskipped pool, in pool then asset order.
func revenueReserveKeys(in protocolRevenueInput, maxBucket int) []revenueReserveKey {
	order := map[string]int{}
	for i, p := range in.pools {
		if _, covered := revenueCovered(in.covs, p); covered {
			order[p.pool] = i
		}
	}
	wins := map[[2]string]*revenueReserveWin{}
	for _, g := range in.sums {
		if _, ok := order[g.Pool]; !ok || g.Bucket > maxBucket {
			continue
		}
		k := [2]string{g.Pool, g.Asset}
		r := wins[k]
		if r == nil {
			r = &revenueReserveWin{}
			wins[k] = r
		}
		r.addGroup(g)
	}
	out := make([]revenueReserveKey, 0, len(wins))
	for k, r := range wins {
		out = append(out, revenueReserveKey{pool: k[0], asset: k[1], win: r})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].pool != out[j].pool {
			return order[out[i].pool] < order[out[j].pool]
		}
		return out[i].asset < out[j].asset
	})
	return out
}

// protocolRevenuePools are a protocol's listed pools: a pool whose reserves could not be read is left out, as no
// read-time verdict could be applied to its reserves.
func protocolRevenuePools(protocol string, reads []protocolPoolRead) []revenuePool {
	var out []revenuePool
	for i := range reads {
		pr := &reads[i]
		if pr.protocol != protocol {
			continue
		}
		p := revenuePool{pool: pr.pool}
		switch {
		case pr.omitted != "":
			p.skip = exclusionPoolReadFailed
		case protocol == blnt.SourceName:
		case pr.res.version == blend.PoolV1:
			p.version = "v1"
		case pr.res.version == blend.PoolV2:
			p.version = "v2"
		default:
			p.skip = protocolRevenueVersionUnknown
		}
		out = append(out, p)
	}
	return out
}

// readProtocolRevenue builds every protocol's revenue block from one coverage read, one GROUP BY and one verdict per
// asset, all within protocolRevenueBudget. A failed or timed-out read is named in the blocks and marks them degraded; it
// never makes the rest of the build partial.
func (s *Server) readProtocolRevenue(ctx context.Context, reads []protocolPoolRead, blntListFailed bool) map[string]LendingProtocolRevenue {
	ins := map[string]*protocolRevenueInput{}
	for _, pt := range lendingProtocols {
		ins[pt.protocol] = &protocolRevenueInput{
			fee: pt.protocol == blnt.SourceName, versioned: pt.protocol != blnt.SourceName,
			listFailed: pt.protocol == blnt.SourceName && blntListFailed,
			pools:      protocolRevenuePools(pt.protocol, reads), verdicts: map[string]string{},
		}
	}
	bctx, cancel := context.WithTimeout(ctx, protocolRevenueBudget)
	defer cancel()
	failed, reason := s.fillProtocolRevenue(bctx, ins), protocolRevenueReadFailed
	if bctx.Err() != nil {
		failed, reason = true, protocolRevenueTimedOut
	}
	out := map[string]LendingProtocolRevenue{}
	for p, in := range ins {
		if failed {
			for i := range in.pools {
				if in.pools[i].skip == "" {
					in.pools[i].skip = reason
				}
			}
		}
		out[p] = foldProtocolRevenue(*in)
	}
	return out
}

// fillProtocolRevenue reads the coverage, the sums and the verdicts into ins; true when the stored days could not be
// read. Without a reader (no Postgres) the inputs stay empty and every pool reads not_valued.
func (s *Server) fillProtocolRevenue(ctx context.Context, ins map[string]*protocolRevenueInput) bool {
	rd, ok := s.ProtocolStats.(lendingRevenueUSDWindowReader)
	if !ok {
		return false
	}
	covs, err := rd.LendingRevenueUSDCoverages(ctx)
	if err != nil {
		s.logger.Warn("lending protocols: revenue coverage read failed", "err", err)
		return true
	}
	var spans []timescale.LendingRevenueUSDSpan
	for _, in := range ins {
		in.covs = covs
		_, through, ok := protocolRevenueSpan(in.pools, covs)
		if !ok {
			continue
		}
		for _, p := range in.pools {
			if c, covered := revenueCovered(covs, p); covered {
				spans = append(spans, timescale.LendingRevenueUSDSpan{Pool: p.pool, FromDay: c.FloorDay, ThroughDay: minTime(c.ThroughDay, through), WindowEnd: through})
			}
		}
	}
	sums, err := rd.LendingRevenueUSDBucketSums(ctx, spans, lendingRevenueValuationRev, int16(lendinginterest.DeriveRev), exclusionPriceWithheld) //nolint:gosec // a small revision number
	if err != nil {
		s.logger.Warn("lending protocols: revenue sums read failed", "err", err)
		return true
	}
	byPool := map[string]*protocolRevenueInput{}
	for _, in := range ins {
		for _, p := range in.pools {
			byPool[p.pool] = in
		}
	}
	for _, g := range sums {
		if in := byPool[g.Pool]; in != nil {
			in.sums = append(in.sums, g)
		}
	}
	verdicts := s.revenueVerdicts(ctx, sums)
	for _, in := range ins {
		in.verdicts = verdicts
	}
	return false
}

// revenueVerdicts is each asset's read-time refusal (withheld or price_read_failed), applied through the walk /revenue
// and the rollup value with ([Server.reserveUSDWalk]: issuer check, operator withholding, thin latest market): one walk
// per distinct asset, of its latest stored day carrying an amount, the day whose newest bucket every window ends on.
func (s *Server) revenueVerdicts(ctx context.Context, sums []timescale.LendingRevenueUSDBucketSum) map[string]string {
	last := map[string]time.Time{}
	var assets []string
	for _, g := range sums {
		d, seen := last[g.Asset]
		if !seen {
			assets = append(assets, g.Asset)
		}
		if !seen || g.LastAmountDay.After(d) {
			last[g.Asset] = g.LastAmountDay
		}
	}
	reasons := make([]string, len(assets))
	forEachBounded(s.logger, len(assets), readFanoutConcurrency, func(i int) {
		if d := last[assets[i]]; !d.IsZero() { // no amount: nothing to value, nothing to refuse
			reasons[i] = usdReadRefusal(s.revenueDayRead(ctx, assets[i], d, 0)) // distinct indices
		}
	})
	out := map[string]string{}
	for i, a := range assets {
		if reasons[i] != "" {
			out[a] = reasons[i]
		}
	}
	return out
}
