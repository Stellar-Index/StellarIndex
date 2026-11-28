package v1

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// lendingRateDaysReader is the lake read behind /v1/lending/yields/history.
type lendingRateDaysReader interface {
	LendingRateDays(ctx context.Context, pool, asset string, from, to time.Time, cov clickhouse.LendingHistoryCoverage) (clickhouse.LendingRateDays, error)
}

var _ lendingRateDaysReader = (*clickhouse.ExplorerReader)(nil)

// lendingEmissionsDaysReader is the emissions rollup's stored days.
type lendingEmissionsDaysReader interface {
	LendingEmissionsUSDCoverages(ctx context.Context) (map[string]timescale.LendingEmissionsUSDCoverage, error)
	LendingEmissionsUSDDays(ctx context.Context, pool string, from, through time.Time) ([]timescale.LendingEmissionsUSDDay, error)
}

var _ lendingEmissionsDaysReader = (*timescale.Store)(nil)

const (
	lendingYieldHistoryDefaultWindow = 90 * day24
	// lendingYieldHistoryMaxWindow is the reserve history's daily cap: one series never holds more than 1998 days.
	lendingYieldHistoryMaxWindow = lendingHistoryMaxDailyWindow
	// lendingYieldHistoryCostDays prices every request, cached or not: one rate-limit token per 90 days started.
	lendingYieldHistoryCostDays   = 90
	lendingYieldHistoryMaxEntries = 64
	// lendingYieldHistoryWaitMargin is how long past the reads' budget the response waits for the fill to assemble them.
	lendingYieldHistoryWaitMargin = 500 * time.Millisecond
	// emissionsBasisRollupAtClose: a day's stored USD x 365 over the side's USD at the day's close.
	emissionsBasisRollupAtClose = "rollup_day_usd_over_side_usd_at_close"
)

// LendingYieldHistoryView is the GET /v1/lending/yields/history body: one reserve side, one point per complete UTC day the
// reserve-state history is derived through.
type LendingYieldHistoryView struct {
	Pool     string  `json:"pool"`
	Asset    string  `json:"asset"`
	AssetID  *string `json:"asset_id"`
	Protocol string  `json:"protocol"`
	Side     string  `json:"side"`
	Interval string  `json:"interval"`
	// From and To are the requested range aligned to UTC days; ServedThrough is the end of the last day served, null
	// when none is (the history is not derived past from).
	From          WireTime  `json:"from"`
	To            WireTime  `json:"to"`
	ServedThrough *WireTime `json:"served_through"`
	lendingHistoryCoverageView
	APYConvention  string `json:"apy_convention"`
	EmissionsBasis string `json:"emissions_basis"`
	// EmissionsThroughDay is the last day the emissions rollup has stored for the pool; null when it holds none.
	EmissionsThroughDay   *string                    `json:"emissions_through_day"`
	EmissionToken         *string                    `json:"emission_token"`
	EmissionTokenDecimals *int                       `json:"emission_token_decimals"`
	Points                []LendingYieldHistoryPoint `json:"points"`
}

type yieldHistoryQuery struct {
	pool, side string
	forms      lendingRatesQuery // the asset filter's alias spellings
	from, to   time.Time
}

// yieldHistoryInputs is one series' reads, both sides alike.
type yieldHistoryInputs struct {
	days         clickhouse.LendingRateDays
	statesReason string
	emis         yieldHistoryEmissions
	prices       reserveUSDSeries
	token        *string
	decimals     *int
	degraded     bool
}

var lendingYieldHistoryCaches sync.Map

func (s *Server) yieldHistoryCache() *swrCache[yieldHistoryInputs] {
	return perServer(&lendingYieldHistoryCaches, s, func() *swrCache[yieldHistoryInputs] {
		return newSWRCache[yieldHistoryInputs](s.logger, "lending_yield_history", "api-lending-yield-history-fill", lendingYieldTTL, lendingYieldHistoryMaxEntries).
			withTTLOf(func(in yieldHistoryInputs) time.Duration {
				if in.degraded {
					return lendingYieldRetryTTL
				}
				return lendingYieldTTL
			})
	})
}

func parseYieldHistoryQuery(w http.ResponseWriter, r *http.Request) (yieldHistoryQuery, bool) {
	v := r.URL.Query()
	q := yieldHistoryQuery{pool: v.Get("pool"), side: v.Get("side")}
	if !canonical.IsContractID(q.pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool", "Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return q, false
	}
	a, err := canonical.ParseAsset(v.Get("asset"))
	if err != nil {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset", "Invalid asset", http.StatusBadRequest,
			"asset must be a canonical asset id or token contract C-strkey")
		return q, false
	}
	q.forms.assetForms = map[string]struct{}{}
	for _, f := range canonical.AssetAliasStrings(a) {
		q.forms.assetForms[f] = struct{}{}
	}
	switch q.side {
	case "":
		q.side = clickhouse.EmissionSideSupply
	case clickhouse.EmissionSideSupply, clickhouse.EmissionSideBorrow:
	default:
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter", "Invalid parameter", http.StatusBadRequest, "side must be supply or borrow")
		return q, false
	}
	if iv := v.Get("interval"); iv != "" && iv != "day" {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-interval", "Invalid interval", http.StatusBadRequest, "interval must be day")
		return q, false
	}
	from, to, ok := parseFromTo(w, r)
	if !ok {
		return q, false
	}
	if v.Get("from") == "" && v.Get("window") == "" {
		from = to.Add(-lendingYieldHistoryDefaultWindow)
	}
	q.from, q.to = from.UTC().Truncate(day24), to.UTC().Truncate(day24)
	switch {
	case !q.from.Before(q.to):
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-window", "Window holds no whole day", http.StatusBadRequest,
			"from and to must span at least one whole UTC day")
		return q, false
	case q.to.Sub(q.from) > lendingYieldHistoryMaxWindow:
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-window", "Window too wide for interval=day", http.StatusBadRequest,
			"interval=day allows at most a "+strconv.Itoa(int(lendingYieldHistoryMaxWindow/day24))+"-day window")
		return q, false
	}
	return q, true
}

// handleLendingYieldsHistory serves GET /v1/lending/yields/history: per UTC day, the realised rate between the day's bounding
// states, the utilisation at its close and the emissions the rollup stored for it.
func (s *Server) handleLendingYieldsHistory(w http.ResponseWriter, r *http.Request) {
	q, ok := parseYieldHistoryQuery(w, r)
	if !ok {
		return
	}
	if s.Explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	protocol, _, ok := s.lendingHistoryLineage(ctx, w, r, q.pool)
	if !ok {
		return
	}
	row, ok := s.yieldHistoryReserve(ctx, w, r, protocol, q)
	if !ok || !middleware.ChargeRateLimit(w, r, 1+int((q.to.Sub(q.from)-1)/(lendingYieldHistoryCostDays*day24))) {
		return
	}
	var flags Flags
	cov, base := s.yieldCoverage(ctx, &flags)
	end := minTime(q.to, time.Now().UTC().Truncate(day24)) // a day still open is never served, whatever coverage says
	if base.statesReason == "" {
		end = minTime(end, cov.ThroughTime.UTC().Truncate(time.Hour).Truncate(day24))
		flags.Stale = flags.Stale || cov.ThroughTime.Before(time.Now().Add(-lendingYieldHistoryLag))
	}
	view := LendingYieldHistoryView{
		Pool: q.pool, Asset: row.Asset, AssetID: row.AssetID, Protocol: protocol, Side: q.side, Interval: "day",
		From: WireTime(q.from), To: WireTime(q.to), lendingHistoryCoverageView: coverageView(cov, 0),
		APYConvention: apyConventionRealised, EmissionsBasis: emissionsBasisRollupAtClose, Points: []LendingYieldHistoryPoint{},
	}
	if end.After(q.from) {
		in := s.readYieldHistory(ctx, yieldHistorySeries{
			protocol: protocol, pool: q.pool, asset: row.Asset, from: q.from, end: end, cov: cov, statesReason: base.statesReason,
		})
		fillYieldHistory(&view, in, q, end)
		flags = in.prices.usdFlags(flags)
		flags.Degraded = flags.Degraded || in.degraded || view.DroppedPoints > 0
	}
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(time.Now().UTC()), Flags: flags})
}

// yieldHistoryReserve is the pool's reserve the asset names in any alias spelling, having written the 404 when none does.
func (s *Server) yieldHistoryReserve(ctx context.Context, w http.ResponseWriter, r *http.Request, protocol string, q yieldHistoryQuery) (LendingRate, bool) {
	pr := s.readRatePool(ctx, ratePool{protocol: protocol, pool: q.pool})
	switch {
	case pr.reason == backstopReasonUnknown:
		writeLendingHistoryNotFound(w, r, "no lending pool with this id is known")
		return LendingRate{}, false
	case pr.omitted != "":
		s.writeLendingReservesTimeout(w, r, "yield history reserves", q.pool)
		return LendingRate{}, false
	}
	for _, rv := range pr.res.view.Reserves {
		var row LendingRate
		s.fillRateAsset(&row, rv.Asset)
		if q.forms.matchesAsset(row) {
			return row, true
		}
	}
	writeLendingHistoryNotFound(w, r, "no reserve of this pool is this asset")
	return LendingRate{}, false
}

// fillYieldHistory sets one point per day in [view.From, end).
func fillYieldHistory(view *LendingYieldHistoryView, in yieldHistoryInputs, q yieldHistoryQuery, end time.Time) {
	byDay := make(map[time.Time]clickhouse.LendingRateDay, len(in.days.Days))
	for _, d := range in.days.Days {
		byDay[d.Day] = d
		if d.FirstUndecodable || d.LastUndecodable {
			view.DroppedPoints++
		}
	}
	borrow := q.side == clickhouse.EmissionSideBorrow
	for d := q.from; d.Before(end); d = d.Add(day24) {
		p := LendingYieldHistoryPoint{T: WireTime(d)}
		st, ok := bucketRateStates(in.days, byDay, d)
		endState := yieldHistoryRealised(&p, st, ok, d, borrow, in.statesReason)
		in.emis.point(&p, view.Asset, q.side, d, sideUSDAt(endState, borrow, in.prices, d))
		view.Points = append(view.Points, p)
	}
	served := WireTime(end)
	view.ServedThrough = &served
	view.EmissionToken, view.EmissionTokenDecimals = in.token, in.decimals
	if !in.emis.cov.ThroughDay.IsZero() {
		view.EmissionsThroughDay = strRef(in.emis.cov.ThroughDay.Format(time.DateOnly))
	}
}

// readYieldHistory reads a series' inputs through the cache. Each read stops at lendingYieldReadBudget and names its own
// timeout, so the wait is that plus a margin: only a fill that overruns its reads' budgets nulls every figure here.
func (s *Server) readYieldHistory(ctx context.Context, sr yieldHistorySeries) yieldHistoryInputs {
	key := sr.pool + "|" + sr.asset + "|" + strconv.FormatInt(sr.from.Unix(), 10) + "|" + strconv.FormatInt(sr.end.Unix(), 10) + "|" +
		strconv.FormatInt(sr.cov.ThroughTime.Truncate(time.Hour).Unix(), 10) + "|" + sr.statesReason
	budget := lendingYieldReadBudget // read once here: the fill outlives the request
	bctx, cancel := context.WithTimeout(ctx, budget+lendingYieldHistoryWaitMargin)
	defer cancel()
	in, _, _, err := s.yieldHistoryCache().get(bctx, "yield_history", key, func(fctx context.Context) (yieldHistoryInputs, error) {
		return s.fillYieldHistoryInputs(fctx, sr, budget), nil
	})
	if err != nil {
		return yieldHistoryInputs{
			statesReason: yieldReasonTimedOut, emis: yieldHistoryEmissions{reason: emissionsTimedOut},
			prices: reserveUSDSeries{reason: emissionsTimedOut}, degraded: true,
		}
	}
	return in
}

// fillYieldHistoryInputs runs the series' three reads side by side, each within lendingYieldReadBudget: the reserve's day
// states (one lake query), the pool's stored emission days, and the reserve token's daily USD walk.
func (s *Server) fillYieldHistoryInputs(ctx context.Context, sr yieldHistorySeries, budget time.Duration) yieldHistoryInputs {
	var in yieldHistoryInputs
	var daysDegraded bool
	reads := [...]func(context.Context){
		func(rctx context.Context) { in.days, in.statesReason, daysDegraded = s.readYieldHistoryDays(rctx, sr) },
		func(rctx context.Context) { in.emis, in.token, in.decimals = s.readYieldHistoryEmissions(rctx, sr) },
		func(rctx context.Context) {
			sc := s.reserveDecimals(rctx, clickhouse.BlendReserveState{Asset: sr.asset})
			q := clickhouse.LendingHistoryQuery{From: sr.from, To: sr.end.Add(-time.Hour), Daily: true}
			in.prices = s.reserveUSDWalk(rctx, sr.asset, sc, q, budget).series
		},
	}
	forEachBounded(s.logger, len(reads), len(reads), func(i int) { // each read writes only its own fields
		rctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		reads[i](rctx)
	})
	in.degraded = daysDegraded || in.emis.degraded || in.prices.reason == exclusionPriceReadFailed
	return in
}

// yieldHistorySeries is one series' identity and range; statesReason is set when the history's extent was not read.
type yieldHistorySeries struct {
	protocol, pool, asset string
	from, end             time.Time
	cov                   clickhouse.LendingHistoryCoverage
	statesReason          string
}

// readYieldHistoryDays is the reserve's day states, or the reason, as /rates names it, that they were not read.
func (s *Server) readYieldHistoryDays(ctx context.Context, sr yieldHistorySeries) (clickhouse.LendingRateDays, string, bool) {
	if sr.statesReason != "" {
		return clickhouse.LendingRateDays{}, sr.statesReason, false
	}
	rd, ok := s.Explorer.(lendingRateDaysReader)
	if !ok {
		return clickhouse.LendingRateDays{}, yieldReasonNoReader, false
	}
	days, err := rd.LendingRateDays(ctx, sr.pool, sr.asset, sr.from, sr.end, sr.cov)
	switch {
	case err == nil:
		return days, "", false
	case ctx.Err() != nil:
		return clickhouse.LendingRateDays{}, yieldReasonTimedOut, true
	}
	s.logger.Warn("lending yields history: reserve rate days read failed", "pool", sr.pool, "err", err)
	return clickhouse.LendingRateDays{}, yieldReasonReadFailed, true
}

// readYieldHistoryEmissions reads the pool's stored reserve_schedule days and what judges them now: the coverage, the
// emission token's read-time verdict and the writer's history verdict, as /v1/lending/protocols' emissions block does.
func (s *Server) readYieldHistoryEmissions(ctx context.Context, sr yieldHistorySeries) (yieldHistoryEmissions, *string, *int) {
	rd, ok := s.ProtocolStats.(lendingEmissionsDaysReader)
	if !ok {
		return yieldHistoryEmissions{reason: protocolRevenueNotValued}, nil, nil
	}
	failed := func(err error) (yieldHistoryEmissions, *string, *int) {
		if ctx.Err() != nil {
			return yieldHistoryEmissions{reason: emissionsTimedOut, degraded: true}, nil, nil
		}
		s.logger.Warn("lending yields history: emissions read failed", "pool", sr.pool, "err", err)
		return yieldHistoryEmissions{reason: protocolEmissionsReadFailed, degraded: true}, nil, nil
	}
	covs, err := rd.LendingEmissionsUSDCoverages(ctx)
	if err != nil {
		return failed(err)
	}
	c, covered := emissionsCovered(covs, emisPool{pool: sr.pool})
	if !covered {
		return yieldHistoryEmissions{reason: protocolRevenueNotValued}, nil, nil
	}
	if why, off := c.Unavailable[emisStreamReserves]; off {
		return yieldHistoryEmissions{reason: why, cov: c}, nil, nil
	}
	rows, err := rd.LendingEmissionsUSDDays(ctx, sr.pool, sr.from, sr.end.Add(-day24))
	if err != nil {
		return failed(err)
	}
	e := yieldHistoryEmissions{cov: c, rows: map[time.Time][]timescale.LendingEmissionsUSDDay{}}
	var sums []timescale.LendingEmissionsUSDBucketSum
	var token *string
	var decimals *int
	for _, r := range rows {
		if r.Stream != emisRowSchedule {
			continue
		}
		e.rows[r.Day.UTC()] = append(e.rows[r.Day.UTC()], r)
		if r.ValuationRev == lendingEmissionsValuationRev {
			token, decimals = r.EmissionToken, r.EmissionTokenDecimals
			sums = append(sums, timescale.LendingEmissionsUSDBucketSum{EmissionToken: r.EmissionToken, AmountUSD: r.AmountUSD, Current: true, LastValuedDay: r.Day})
		}
	}
	e.verdicts = s.emissionsVerdicts(ctx, sr.protocol, sums)
	poolSrc, _ := emissionsWriterSources(sr.protocol == blnt.SourceName)
	e.history = s.projectionHistoryVerdicts(ctx, poolSrc)[poolSrc]
	if e.history == historyVerdictUnread {
		e.degraded = true
	}
	return e, token, decimals
}
