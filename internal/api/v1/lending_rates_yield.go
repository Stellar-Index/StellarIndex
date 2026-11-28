package v1

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingRateStatesReader is the lake read of each reserve's ResData around a window, and how far that history is derived.
type lendingRateStatesReader interface {
	LendingHistoryCoverage(ctx context.Context) (clickhouse.LendingHistoryCoverage, error)
	LendingRateStates(ctx context.Context, pool string, assets []string, from, to time.Time, slack time.Duration, cov clickhouse.LendingHistoryCoverage) (clickhouse.LendingRateWindow, error)
}

var _ lendingRateStatesReader = (*clickhouse.ExplorerReader)(nil)

// lendingYieldWindows are the realised windows /v1/lending/rates offers; one applies to the whole response, so the pools it
// lists are compared over the same span.
var lendingYieldWindows = map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

const (
	lendingYieldDefaultWindow = "7d"
	// apyConventionRealised names the one APY served: (r1/r0)^(1/years) - 1 over the realised span, display only.
	apyConventionRealised = "realised"
	// netBasisRealisedCurrent says a net yield adds a realised base over the span to the emission rate in force now.
	netBasisRealisedCurrent = "realised_base_current_emissions"
	// lendingYieldHistoryLag is how far the derived history may trail the request before its span is no longer "now".
	lendingYieldHistoryLag  = 2 * time.Hour
	lendingYieldTTL         = time.Minute
	lendingYieldCoverageTTL = 30 * time.Second
	// lendingYieldRetryTTL holds a degraded or timed-out fill briefly, so a failing lake is not re-read by every request.
	lendingYieldRetryTTL = 15 * time.Second
	// lendingYieldMaxEntries holds every pool the reserves cache can hold, for each of the three windows; a key also names
	// the derived hour, so an hour's turn leaves the old keys to age out.
	lendingYieldMaxEntries = 3 * lendingReservesMaxEntries
	// caveatEmissionsCurrent is on every net that includes an emission stream: the rate is the one in force now, not the
	// window's average, and a change inside the window is not detected.
	caveatEmissionsCurrent = "emission_rate_is_current_not_window_average"
)

// lendingYieldReadBudget bounds how long a request waits for one pool's yield inputs; past it only the yield block degrades.
var lendingYieldReadBudget = 3 * time.Second

// Reasons a realised rate is null, beyond insufficient_resolution.
const (
	yieldReasonNoReader     = "rate_history_reader_unavailable"
	yieldReasonNotDerived   = "rate_history_not_derived"
	yieldReasonReadFailed   = "rate_history_read_failed"
	yieldReasonTimedOut     = "rate_history_timed_out"
	yieldReasonNoAccrual    = "no_accrual_in_window"     // no state, or one state, from an hour before the window to its end
	yieldReasonNoBase       = "no_base_state_in_window"  // the reserve has no state before the window and one inside it
	yieldReasonOldBase      = "base_state_before_window" // its state at the window's start is older than the base slack
	yieldReasonUndecodable  = "state_undecodable"
	yieldReasonRemoved      = "reserve_state_removed"
	yieldReasonShortWindow  = "states_under_one_hour_apart"
	yieldReasonBaseRateZero = "base_rate_not_positive" // b_rate defaulted to 0 has no ratio after it
	yieldReasonLowRes       = "insufficient_resolution"
	yieldReasonRateInvalid  = "rate_invalid"
	emissionPriceThin       = "emission_price_thin_market" // the token has a display-only price, which values nothing
	emissionStreamUnmapped  = "emission_stream_unmapped"   // a stream names no reserve, so an absent one is not a zero
	emissionStreamBad       = "emission_stream_undecodable"
	emissionResListUnread   = "reserve_list_unread"
	emissionsTimedOut       = "emissions_timed_out"
)

// Reasons a yield with a value does not rank, beside blend.Rankable's caveats and the row's exclusions.
const (
	rankHistoryBehind = "rate_history_behind"
	rankShortSpan     = "realised_span_under_half_window"
	rankStreamUnknown = "emission_stream_unknown"
)

// LendingRateYield is a reserve's realised and net yield over the response's yield_window. RealisedFrom and RealisedTo are
// the last_time of the two stored states the rates are read between, and RealisedSpanSeconds the time between them.
type LendingRateYield struct {
	RealisedFrom        *WireTime        `json:"realised_from"`
	RealisedTo          *WireTime        `json:"realised_to"`
	RealisedSpanSeconds *int64           `json:"realised_span_seconds"`
	Supply              LendingSideYield `json:"supply"`
	Borrow              LendingSideYield `json:"borrow"`
}

// LendingSideYield is one side's yield; every rate is a fraction at 4 dp, and every null value carries its reason.
type LendingSideYield struct {
	// RealisedAPR is (r1/r0 - 1) / years from b_rate (supply) or d_rate (borrow), exact before the final rounding.
	RealisedAPR       *string `json:"realised_apr"`
	RealisedAPRReason *string `json:"realised_apr_reason"`
	// RealisedAPY is display only (apy_convention); it is never summed into a net yield.
	RealisedAPY *string `json:"realised_apy"`
	// EmissionsAPR is the side's current emission stream as /v1/lending/pools/{pool}/emissions serves it.
	EmissionsAPR       *string `json:"emissions_apr"`
	EmissionsAPRReason *string `json:"emissions_apr_reason"`
	// EmissionsTokensPer1000USDYear is free of the emission token's price, so it holds when that price is thin.
	EmissionsTokensPer1000USDYear *string `json:"emissions_tokens_per_1000usd_year"`
	// NetYield is realised + emissions (supply) or emissions - realised (borrow); null without a realised rate, and a lower
	// bound, naming what it leaves out, without emissions.
	NetYield        *string                 `json:"net_yield"`
	NetBasis        string                  `json:"net_basis"`
	NetLowerBound   bool                    `json:"net_lower_bound"`
	NetExcluded     []LendingYieldExclusion `json:"net_excluded"`
	NetCaveats      []string                `json:"net_caveats"`
	Rankable        bool                    `json:"rankable"`
	RankableReasons []string                `json:"rankable_reasons"`
	// netExact is NetYield before rounding: what an ordering compares.
	netExact *big.Rat
}

// LendingYieldExclusion is a component left out of a net yield.
type LendingYieldExclusion struct {
	Kind   string `json:"kind"` // base | emissions
	Reason string `json:"reason"`
}

// rateYieldInputs is what one pool's yield columns are computed from, read once per pool, window and derived hour.
type rateYieldInputs struct {
	from         time.Time     // the window's start the states were read around
	window       time.Duration // the window's length, which sets the base slack
	states       map[string]clickhouse.LendingRateStates
	statesReason string // every reserve's realised_apr_reason when the states were not read
	streams      map[string]LendingReserveEmissionView
	undecodable  map[string]bool // asset+side of a stream entry that exists but failed to decode
	resList      bool            // the pool's reserve list was read, so an absent stream is known absent
	unmapped     bool            // a stream names no reserve asset
	emisReason   string          // every reserve's emissions_apr_reason when the streams were not read
	price        kpiTokenPrice
	emisDecimals *int
	backstopDebt map[string]*big.Int // the backstop's d-tokens per reserve; nil when unknown
	degraded     bool
}

// rateYieldRequest is what every pool's yield shares in one request.
type rateYieldRequest struct {
	window  time.Duration
	behind  bool // the derived history ends more than lendingYieldHistoryLag before the request
	through *WireTime
}

// lendingRateYieldCaches and lendingYieldCoverageCaches hold each server's caches; the server struct takes no new fields.
var lendingRateYieldCaches, lendingYieldCoverageCaches sync.Map

func perServer[V any](m *sync.Map, s *Server, mk func() *V) *V {
	if c, ok := m.Load(s); ok {
		return c.(*V)
	}
	got, _ := m.LoadOrStore(s, mk())
	return got.(*V)
}

func (s *Server) rateYieldCache() *swrCache[rateYieldInputs] {
	return perServer(&lendingRateYieldCaches, s, func() *swrCache[rateYieldInputs] {
		return newSWRCache[rateYieldInputs](s.logger, "lending_rate_yield", "api-lending-rate-yield-fill", lendingYieldTTL, lendingYieldMaxEntries).
			withTTLOf(func(in rateYieldInputs) time.Duration {
				if in.degraded {
					return lendingYieldRetryTTL
				}
				return lendingYieldTTL
			})
	})
}

// yieldCoverageRead is a cached read of the derived history's extent; reason is set when it cannot be served.
type yieldCoverageRead struct {
	cov      clickhouse.LendingHistoryCoverage
	reason   string
	degraded bool
}

func (s *Server) yieldCoverageCache() *swrCache[yieldCoverageRead] {
	return perServer(&lendingYieldCoverageCaches, s, func() *swrCache[yieldCoverageRead] {
		return newSWRCache[yieldCoverageRead](s.logger, "lending_rate_yield_coverage", "api-lending-rate-yield-coverage-fill", lendingYieldCoverageTTL, 1).
			withTTLOf(func(r yieldCoverageRead) time.Duration {
				if r.degraded {
					return lendingYieldRetryTTL
				}
				return lendingYieldCoverageTTL
			})
	})
}

func parseYieldWindow(w http.ResponseWriter, r *http.Request) (string, time.Duration, bool) {
	name := r.URL.Query().Get("window")
	if name == "" {
		name = lendingYieldDefaultWindow
	}
	d, ok := lendingYieldWindows[name]
	if !ok {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
			"Invalid parameter", http.StatusBadRequest, "window must be 24h, 7d or 30d")
	}
	return name, d, ok
}

// readRateYields reads the yield inputs of every pool that has rows, after the reserves reads and in their own fan-out, each
// through the per-(pool, window, derived hour) cache and within lendingYieldReadBudget.
func (s *Server) readRateYields(ctx context.Context, reads []ratePoolRead, rows [][]LendingRate, q lendingRatesQuery, now time.Time) ([]rateYieldInputs, rateYieldRequest, Flags) {
	req := rateYieldRequest{window: q.window}
	var flags Flags
	cov, base := s.yieldCoverage(ctx, &flags)
	if base.statesReason == "" {
		through := WireTime(cov.ThroughTime)
		req.through, req.behind = &through, cov.ThroughTime.Before(now.Add(-lendingYieldHistoryLag))
		flags.Stale = flags.Stale || req.behind
	}
	from := now.Add(-q.window)
	out := make([]rateYieldInputs, len(reads))
	var mu sync.Mutex
	forEachBounded(s.logger, len(reads), readFanoutConcurrency, func(i int) {
		if len(rows[i]) == 0 {
			return
		}
		pr := reads[i]
		key := pr.pool + "|" + q.windowName + "|" + strconv.FormatInt(cov.ThroughTime.Truncate(time.Hour).Unix(), 10) + "|" + base.statesReason
		bctx, cancel := context.WithTimeout(ctx, lendingYieldReadBudget)
		defer cancel()
		in, _, _, err := s.rateYieldCache().get(bctx, "rate_yield", key, func(fctx context.Context) (rateYieldInputs, error) {
			return s.fillRateYield(fctx, pr, base.statesReason, from, now, q.window, cov), nil
		})
		if err != nil { // timed out: the fill carries on detached and the next request reads it
			in = rateYieldInputs{statesReason: yieldReasonTimedOut, emisReason: emissionsTimedOut, degraded: true}
		}
		out[i] = in
		f := in.flags()
		mu.Lock()
		flags.Degraded, flags.Stale = flags.Degraded || f.Degraded, flags.Stale || f.Stale
		mu.Unlock()
	})
	return out, req, flags
}

// flags is what a pool's inputs add to the response flags: a failed read degrades it, and a stale emission price that
// valued a stream makes it stale, as on /emissions.
func (in rateYieldInputs) flags() Flags {
	return Flags{Degraded: in.degraded, Stale: in.price.reason == "" && in.price.stale}
}

// yieldCoverage reads the derived history's extent through its cache; base.statesReason is set when it cannot be.
func (s *Server) yieldCoverage(ctx context.Context, flags *Flags) (clickhouse.LendingHistoryCoverage, rateYieldInputs) {
	sr, ok := s.Explorer.(lendingRateStatesReader)
	if !ok {
		return clickhouse.LendingHistoryCoverage{}, rateYieldInputs{statesReason: yieldReasonNoReader}
	}
	cctx, cancel := context.WithTimeout(ctx, lendingYieldReadBudget)
	defer cancel()
	r, _, _, err := s.yieldCoverageCache().get(cctx, "rate_yield_coverage", "coverage", func(fctx context.Context) (yieldCoverageRead, error) {
		fctx, cancel := context.WithTimeout(fctx, lendingYieldReadBudget)
		defer cancel()
		cov, err := sr.LendingHistoryCoverage(fctx)
		switch {
		case errors.Is(err, clickhouse.ErrLendingHistoryNotReady):
			return yieldCoverageRead{cov: cov, reason: yieldReasonNotDerived}, nil
		case fctx.Err() != nil:
			return yieldCoverageRead{reason: yieldReasonTimedOut, degraded: true}, nil
		case err != nil:
			s.logger.Warn("lending rates: rate history coverage read failed", "err", err)
			return yieldCoverageRead{reason: yieldReasonReadFailed, degraded: true}, nil
		}
		return yieldCoverageRead{cov: cov}, nil
	})
	if err != nil {
		r = yieldCoverageRead{reason: yieldReasonTimedOut, degraded: true}
	}
	flags.Degraded = flags.Degraded || r.degraded
	return r.cov, rateYieldInputs{statesReason: r.reason}
}

// fillRateYield reads a pool's yield inputs within lendingYieldReadBudget: the reserve states around the window, the
// emission streams and their token's price, and the backstop's debt per reserve. A fill that runs out of budget is cached
// as timed out for lendingYieldRetryTTL, so a slow lake costs one wait per pool, not one per request.
func (s *Server) fillRateYield(ctx context.Context, pr ratePoolRead, statesReason string, from, to time.Time, window time.Duration, cov clickhouse.LendingHistoryCoverage) rateYieldInputs {
	ctx, cancel := context.WithTimeout(ctx, lendingYieldReadBudget)
	defer cancel()
	in := rateYieldInputs{from: from, window: window, statesReason: statesReason}
	if statesReason == "" {
		s.readYieldStates(ctx, pr, from, to, window, cov, &in)
	}
	s.readYieldEmissions(ctx, pr, to, &in)
	in.backstopDebt = s.readBackstopDebt(ctx, pr, &in)
	if ctx.Err() != nil {
		return rateYieldInputs{from: from, window: window, statesReason: yieldReasonTimedOut, emisReason: emissionsTimedOut, degraded: true}
	}
	return in
}

func (s *Server) readYieldStates(ctx context.Context, pr ratePoolRead, from, to time.Time, window time.Duration, cov clickhouse.LendingHistoryCoverage, in *rateYieldInputs) {
	sr, ok := s.Explorer.(lendingRateStatesReader)
	if !ok {
		in.statesReason = yieldReasonNoReader
		return
	}
	assets := make([]string, 0, len(pr.res.view.Reserves))
	for _, rv := range pr.res.view.Reserves {
		assets = append(assets, rv.Asset)
	}
	w, err := sr.LendingRateStates(ctx, pr.pool, assets, from, to, clickhouse.LendingRateBaseSlack(window), cov)
	if err != nil {
		s.logger.Warn("lending rates: reserve rate states read failed", "pool", pr.pool, "err", err)
		in.statesReason, in.degraded = yieldReasonReadFailed, true
		return
	}
	in.states = w.Reserves
}

func (s *Server) readYieldEmissions(ctx context.Context, pr ratePoolRead, asOf time.Time, in *rateYieldInputs) {
	if pr.protocol != blnt.SourceName && pr.res.version == blend.PoolV1 {
		in.emisReason = emissionsReasonV1
		return
	}
	view := LendingEmissionsView{Pool: pr.pool, Protocol: pr.protocol, AsOfTime: WireTime(asOf)}
	var flags Flags
	if err := s.fillEmissionStreams(ctx, &view, pr.protocol == blnt.SourceName, &flags); err != nil {
		s.logger.Warn("lending rates: emission streams read failed", "pool", pr.pool, "err", err)
		in.emisReason, in.degraded = protocolEmissionsReadFailed, true
		return
	}
	in.degraded = in.degraded || flags.Degraded
	if !view.StorageAvailable {
		in.emisReason = emissionsReasonNoReader
		if view.StorageUnavailableReason != nil {
			in.emisReason = *view.StorageUnavailableReason
		}
		return
	}
	token := ""
	if view.EmissionToken != nil {
		token = *view.EmissionToken
	}
	in.price, in.emisDecimals = s.emissionTokenPrice(ctx, pr.protocol, token, view.EmissionTokenDecimals), view.EmissionTokenDecimals
	in.resList = view.reserveList != nil
	in.streams = make(map[string]LendingReserveEmissionView, len(view.Reserves))
	in.undecodable = map[string]bool{}
	for _, id := range view.undecodable {
		if asset, side, ok := clickhouse.ReserveTokenSide(id, view.reserveList); ok {
			in.undecodable[asset+"\x00"+side] = true
		}
	}
	for _, re := range view.Reserves {
		if re.Asset == nil {
			in.unmapped = true
			continue
		}
		in.streams[*re.Asset+"\x00"+re.Side] = re
	}
}

// readBackstopDebt is the d-tokens the pool's backstop owes per reserve: debt moved there keeps accruing into b_rate though
// no one may repay it. Nil when it is not known, as on BLNT, whose debt-carrying address is unverified.
func (s *Server) readBackstopDebt(ctx context.Context, pr ratePoolRead, in *rateYieldInputs) map[string]*big.Int {
	if pr.protocol == blnt.SourceName || pr.res.instance == nil || pr.res.instance.Backstop == "" {
		return nil
	}
	posr, ok := s.Explorer.(lendingPositionsReader)
	if !ok {
		return nil
	}
	pos, err := posr.LendingAccountPositions(ctx, pr.pool, pr.res.instance.Backstop)
	if err != nil {
		s.logger.Warn("lending rates: backstop position read failed", "pool", pr.pool, "err", err)
		in.degraded = true
		return nil
	}
	if !pos.Readable {
		return nil
	}
	if pos.Borrow == nil {
		return map[string]*big.Int{}
	}
	return pos.Borrow
}

// rateYield computes one reserve's yield from its pool's inputs; row carries the pool status and exclusions, rv the
// reserve's current view, data its raw current state.
func rateYield(in rateYieldInputs, req rateYieldRequest, row LendingRate, rv ReserveView, data *blend.ReserveData) LendingRateYield {
	var out LendingRateYield
	base, end, pairReason := in.realisedPair(rv.Asset)
	var span time.Duration
	if pairReason == "" {
		from, to := WireTime(base.LastTime), WireTime(end.LastTime)
		span = end.LastTime.Sub(base.LastTime)
		secs := int64(span / time.Second)
		out.RealisedFrom, out.RealisedTo, out.RealisedSpanSeconds = &from, &to, &secs
	}
	facts := collectabilityFacts(in, row, rv.Asset, data)
	blocks := rowRankBlocks(row, req, span, pairReason == "")
	supply := realisedSide(base, end, pairReason, false)
	borrow := realisedSide(base, end, pairReason, true)
	out.Supply = in.sideYield(blend.YieldSupply, clickhouse.EmissionSideSupply, rv, supply, facts, blocks)
	out.Borrow = in.sideYield(blend.YieldBorrow, clickhouse.EmissionSideBorrow, rv, borrow, facts, blocks)
	return out
}

// rowRankBlocks are the reasons every side of a row stays out of a ranking: a reserve no new position can enter, a history
// that trails the request, or a realised span under half the window.
func rowRankBlocks(row LendingRate, req rateYieldRequest, span time.Duration, realised bool) []string {
	out := append([]string(nil), row.Exclusions...)
	if req.behind {
		out = append(out, rankHistoryBehind)
	}
	if realised && span < req.window/2 {
		out = append(out, rankShortSpan)
	}
	return slices.Clip(out) // each side appends its own reasons
}

// realisedPair is the two states a realised rate runs between, or why there are none. A base must have accrued within
// the window's base slack of its start; else the first state inside the window is the base, so the span never averages
// months before it.
func (in rateYieldInputs) realisedPair(asset string) (base, end *clickhouse.LendingRateState, reason string) {
	if in.statesReason != "" {
		return nil, nil, in.statesReason
	}
	st, ok := in.states[asset]
	return realisedStatePair(st, ok, in.from, in.window)
}

// realisedStatePair is realisedPair over one reserve's states read around a window opening at from; ok is false when the
// read held no state for the reserve. /v1/lending/yields/history applies it to each day.
func realisedStatePair(st clickhouse.LendingRateStates, ok bool, from time.Time, window time.Duration) (base, end *clickhouse.LendingRateState, reason string) {
	switch {
	case ok && st.Undecodable:
		return nil, nil, yieldReasonUndecodable
	case !ok || st.End == nil:
		return nil, nil, yieldReasonNoAccrual
	case st.End.Removed:
		return nil, nil, yieldReasonRemoved
	}
	base, old := st.Base, st.Prior
	if base != nil && !base.Removed && base.LastTime.Before(from.Add(-clickhouse.LendingRateBaseSlack(window))) {
		base, old = nil, true
	}
	if base == nil {
		switch {
		case st.First != nil && st.First.Ledger != st.End.Ledger:
			base = st.First
		case old:
			return nil, nil, yieldReasonOldBase
		default:
			return nil, nil, yieldReasonNoBase
		}
	}
	switch {
	case base.Removed:
		return nil, nil, yieldReasonRemoved
	case base.Ledger == st.End.Ledger:
		return nil, nil, yieldReasonNoAccrual
	}
	return base, st.End, ""
}

// realisedRate is one side's realised APR and display APY between the two states, or the reason it is null.
type realisedRate struct {
	apr    *big.Rat
	apy    *string
	reason string
}

func realisedSide(base, end *clickhouse.LendingRateState, pairReason string, borrow bool) realisedRate {
	if pairReason != "" {
		return realisedRate{reason: pairReason}
	}
	r0, r1 := base.BRate, end.BRate
	if borrow {
		r0, r1 = base.DRate, end.DRate
	}
	t0, t1 := base.LastTime, end.LastTime
	switch {
	case t1.Sub(t0) < blend.MinRealisedInterval:
		return realisedRate{reason: yieldReasonShortWindow}
	case r0 == nil || r0.Sign() <= 0:
		return realisedRate{reason: yieldReasonBaseRateZero}
	}
	apr, err := blend.RealisedAPR(r0, r1, t0, t1)
	switch {
	case errors.Is(err, blend.ErrLowResolution):
		return realisedRate{reason: yieldReasonLowRes}
	case err != nil:
		return realisedRate{reason: yieldReasonRateInvalid}
	}
	out := realisedRate{apr: apr}
	if apy, err := blend.RealisedAPY(r0, r1, t0, t1); err == nil {
		out.apy = &apy
	}
	return out
}

// collectabilityFacts is what the pool and reserve say about whether an owed yield can be withdrawn.
func collectabilityFacts(in rateYieldInputs, row LendingRate, asset string, data *blend.ReserveData) blend.CollectabilityFacts {
	f := blend.CollectabilityFacts{PoolStatusKnown: row.PoolStatus != nil}
	if f.PoolStatusKnown {
		f.PoolActive = *row.PoolStatus == "active" || *row.PoolStatus == "admin_active"
	}
	if data != nil && data.BRate != nil && data.DRate != nil && data.BSupply != nil && data.DSupply != nil {
		f.Utilization = data.Utilization()
	}
	if in.backstopDebt != nil {
		d := in.backstopDebt[asset]
		holds := d != nil && d.Sign() > 0
		f.BackstopHoldsLiabilities = &holds
	}
	return f
}

// sideEmissions is a side's emissions APR (or the reason it has none), its price-free token rate and its stream.
type sideEmissions struct {
	value  *big.Rat
	reason string
	tokens *string
	stream *LendingReserveEmissionView
}

// emissions is zero only when the pool's stream list is complete and holds no stream for this side.
func (in rateYieldInputs) emissions(asset, side string, rv ReserveView) sideEmissions {
	if in.emisReason != "" {
		return sideEmissions{reason: in.emisReason}
	}
	key := asset + "\x00" + side
	st, ok := in.streams[key]
	switch {
	case ok:
	case !in.resList:
		return sideEmissions{reason: emissionResListUnread}
	case in.undecodable[key]:
		return sideEmissions{reason: emissionStreamBad}
	case in.unmapped:
		return sideEmissions{reason: emissionStreamUnmapped}
	default:
		return sideEmissions{value: new(big.Rat), tokens: strRef("0")}
	}
	eps := streamEPS(st.LendingEmissionStreamView)
	out := sideEmissions{tokens: emissionTokensPer1000USDYear(eps, st.Live, side, rv, in.emisDecimals), stream: &st}
	p := in.price
	if p.thin {
		p.reason = emissionPriceThin
	}
	out.value, out.reason = emissionAPRRat(eps, st.Live, side, rv, true, p)
	return out
}

func (in rateYieldInputs) sideYield(side blend.YieldSide, emisSide string, rv ReserveView, rr realisedRate, facts blend.CollectabilityFacts, blocks []string) LendingSideYield {
	out := LendingSideYield{RealisedAPY: rr.apy, NetBasis: netBasisRealisedCurrent, NetExcluded: []LendingYieldExclusion{}, NetCaveats: []string{}}
	base := blend.YieldPart{Value: rr.apr, Reason: rr.reason}
	if rr.apr != nil {
		out.RealisedAPR = strRef(blend.FormatRate(rr.apr))
	} else {
		out.RealisedAPRReason = strRef(rr.reason)
	}
	em := in.emissions(rv.Asset, emisSide, rv)
	out.EmissionsTokensPer1000USDYear = em.tokens
	if em.value != nil {
		out.EmissionsAPR = strRef(blend.FormatRate(em.value))
	} else {
		out.EmissionsAPRReason = strRef(em.reason)
	}
	if em.reason == emissionResListUnread || em.reason == emissionStreamBad || em.reason == emissionStreamUnmapped {
		blocks = append(blocks, rankStreamUnknown) // an unread stream could put this reserve above any other
	}
	net, err := blend.ComposeNetYield(side, base, blend.YieldPart{Value: em.value, Reason: em.reason}, facts)
	if err != nil { // an ambiguous part or a negative emissions APR: no honest net exists, so it is unknown and unranked
		net = blend.NetYield{}
	}
	if net.Value != nil {
		out.NetYield, out.netExact = strRef(blend.FormatRate(net.Value)), net.Value
	}
	out.NetLowerBound = net.LowerBound
	for _, x := range net.Excluded {
		out.NetExcluded = append(out.NetExcluded, LendingYieldExclusion{Kind: string(x.Component), Reason: x.Reason})
	}
	for _, c := range net.Caveats {
		out.NetCaveats = append(out.NetCaveats, string(c))
	}
	if net.Value != nil && em.stream != nil && em.value != nil {
		out.NetCaveats = append(out.NetCaveats, caveatEmissionsCurrent) // informs; ranking is blend.Rankable's
	}
	ok, reasons := blend.Rankable(net)
	out.RankableReasons = make([]string, 0, len(reasons)+len(blocks))
	for _, c := range reasons {
		out.RankableReasons = append(out.RankableReasons, string(c))
	}
	for _, b := range blocks {
		if !slices.Contains(out.RankableReasons, b) {
			out.RankableReasons = append(out.RankableReasons, b)
		}
	}
	out.Rankable = ok && len(out.RankableReasons) == 0
	return out
}
