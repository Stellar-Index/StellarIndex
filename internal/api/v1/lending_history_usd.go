package v1

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// How far before a bucket its price may be: two days for a daily series, as a market-cap day may carry its supply
// ([marketCapSupplyMaxCarry]); six hours for an hourly one. Past it the USD value is null.
const (
	lendingUSDDailyPriceMaxAge  = 48 * time.Hour
	lendingUSDHourlyPriceMaxAge = 6 * time.Hour
)

const (
	// lendingUSDNoPriceInBound: the token has a USD series, but no bucket of it within the price-age bound before this one.
	lendingUSDNoPriceInBound = "no_price_within_bound"
	// lendingUSDArchived: the reserve's state entry is archived; the pool still holds its tokens, at an unknown amount.
	lendingUSDArchived = "archived"
	// priceBasisStablecoinProxy: the price is from a book quoted in a USD stablecoin, not fiat:USD.
	priceBasisStablecoinProxy = "stablecoin_proxy"
)

func lendingUSDPriceMaxAge(q clickhouse.LendingHistoryQuery) time.Duration {
	if q.Daily {
		return lendingUSDDailyPriceMaxAge
	}
	return lendingUSDHourlyPriceMaxAge
}

// LendingHistoryUSD is an amount pair valued at the USD price of its own bucket; null with usd_reason when it is not valued.
type LendingHistoryUSD struct {
	SuppliedUSD *string `json:"supplied_usd"`
	BorrowedUSD *string `json:"borrowed_usd"`
	// PriceUSD is the per-whole-token price used and PriceAt the start of the price bucket it is the VWAP of.
	PriceUSD *string   `json:"price_usd"`
	PriceAt  *WireTime `json:"price_at"`
	// PriceBasis is "transitive" for a price crossed through XLM, "stablecoin_proxy" for one from a stablecoin-quoted book,
	// omitted for one traded against fiat:USD.
	PriceBasis string `json:"price_basis,omitempty"`
	// PriceCarried: the price bucket is more than one bucket older than this one.
	PriceCarried bool    `json:"price_carried,omitempty"`
	USDReason    *string `json:"usd_reason"`
}

// LendingPoolHistoryView is the GET /v1/lending/pools/{pool}/history body: the pool's USD value at each bucket.
type LendingPoolHistoryView struct {
	Pool             string   `json:"pool"`
	Protocol         *string  `json:"protocol"`
	Interval         string   `json:"interval"`
	From             WireTime `json:"from"`
	To               WireTime `json:"to"`
	PriceMaxAgeHours int      `json:"price_max_age_hours"`
	lendingHistoryCoverageView
	// OmittedReserves are pool reserves with no recorded history; every non-null tvl_usd is then a lower bound.
	OmittedReserves []string `json:"omitted_reserves"`
	// ReservesLowerBoundReason is set when the reserve list may miss a reserve; every non-null tvl_usd is then a lower bound.
	ReservesLowerBoundReason *string                   `json:"reserves_lower_bound_reason"`
	Points                   []LendingPoolHistoryPoint `json:"points"`
}

// LendingPoolHistoryPoint is the pool at the close of one bucket, each reserve's state valued at that bucket's price.
type LendingPoolHistoryPoint struct {
	T WireTime `json:"t"`
	// TVLUSD sums the valued reserves' supplied USD; null when none is valued.
	TVLUSD      *string `json:"tvl_usd"`
	BorrowedUSD *string `json:"borrowed_usd"`
	// LowerBound: TVLUSD leaves out the unvalued_reserves or the view's omitted_reserves.
	LowerBound       bool     `json:"lower_bound"`
	UnvaluedReserves []string `json:"unvalued_reserves"`
	// PriceCarriedReserves are the valued reserves whose price is more than one bucket older than this one.
	PriceCarriedReserves []string                    `json:"price_carried_reserves"`
	Reserves             []LendingPoolHistoryReserve `json:"reserves"`
}

// LendingPoolHistoryReserve is one reserve's USD value in a pool history bucket.
type LendingPoolHistoryReserve struct {
	Asset string `json:"asset"`
	LendingHistoryUSD
}

// reserveUSDSeries is one reserve token's USD price series over a history window, read once per request.
type reserveUSDSeries struct {
	dec    int
	points []HistoryPoint // ascending, normalised to the token's price scale
	basis  string
	// stableAt are the price buckets whose owning book is quoted in a stablecoin, not fiat:USD.
	stableAt     map[time.Time]bool
	maxAge, step time.Duration
	// reason is set when no bucket can be valued.
	reason                      string
	proxied, degraded, withheld bool
}

// usdValuation is one state's USD value; reason is set when supplied is nil.
type usdValuation struct {
	supplied, borrowed *big.Rat
	price, basis       string
	at                 time.Time
	carried            bool
	reason             string
}

func (v usdValuation) wire() LendingHistoryUSD {
	if v.supplied == nil {
		return LendingHistoryUSD{USDReason: strRef(v.reason)}
	}
	at := WireTime(v.at.UTC())
	return LendingHistoryUSD{
		SuppliedUSD: strRef(v.supplied.FloatString(2)), BorrowedUSD: strRef(v.borrowed.FloatString(2)),
		PriceUSD: strRef(v.price), PriceAt: &at, PriceBasis: v.basis, PriceCarried: v.carried,
	}
}

// priceAt is the newest price bucket starting at or before bucket, if it is within the series' price-age bound.
func (u reserveUSDSeries) priceAt(bucket time.Time) (HistoryPoint, bool) {
	i := sort.Search(len(u.points), func(i int) bool { return u.points[i].Bucket.After(bucket) })
	if i == 0 {
		return HistoryPoint{}, false
	}
	p := u.points[i-1]
	return p, bucket.Sub(p.Bucket) <= u.maxAge
}

// value prices a reserve state at the bucket starting at bucket, never at a later bucket's price.
func (u reserveUSDSeries) value(st clickhouse.BlendReserveState, bucket time.Time) usdValuation {
	if u.reason != "" {
		return usdValuation{reason: u.reason}
	}
	p, ok := u.priceAt(bucket)
	if !ok {
		return usdValuation{reason: lendingUSDNoPriceInBound}
	}
	sup, errS := usdValueRat(st.Metrics.SuppliedUnderlying, p.VWAP, u.dec)
	bor, errB := usdValueRat(st.Metrics.BorrowedUnderlying, p.VWAP, u.dec)
	switch {
	case errS != nil || errB != nil:
		return usdValuation{reason: backstopUSDUnpriced}
	case sup.Sign() < 0 || bor.Sign() < 0:
		return usdValuation{reason: backstopUSDNegativeAmount}
	}
	basis := u.basis
	if basis == "" && u.stableAt[p.Bucket.UTC()] {
		basis = priceBasisStablecoinProxy
	}
	return usdValuation{supplied: sup, borrowed: bor, price: p.VWAP, basis: basis, at: p.Bucket, carried: bucket.Sub(p.Bucket) > u.step}
}

// reserveUSDSeriesFor reads a reserve token's USD series for q through [Server.reservePriceUSD]'s identity and gates: the SAC
// unwrap, the reserve-decimals scale check and the issuer scam check, then the chart's alias and stablecoin walk.
func (s *Server) reserveUSDSeriesFor(ctx context.Context, assetC string, sc reserveScale, q clickhouse.LendingHistoryQuery) reserveUSDSeries {
	return s.reserveUSDWalk(ctx, assetC, sc, q, 0).series
}

// reserveUSDRead is a reserve's USD series and whether a read-time verdict (a flagged issuer, an operator withholding, a
// thin latest market) refused it, as opposed to the price data or a read: series.reason is withheld then.
type reserveUSDRead struct {
	series  reserveUSDSeries
	verdict bool
}

// reserveUSDWalk is [Server.reserveUSDSeriesFor] with its verdicts told apart; budget bounds the walk's reads once it
// holds a series, zero meaning the request budget.
func (s *Server) reserveUSDWalk(ctx context.Context, assetC string, sc reserveScale, q clickhouse.LendingHistoryQuery, budget time.Duration) reserveUSDRead {
	asset, reason, degraded := s.reserveUSDIdentity(ctx, assetC, sc)
	if reason != "" {
		return reserveUSDRead{series: reserveUSDSeries{reason: reason, degraded: degraded}}
	}
	if flagged, unread := s.reserveIssuerFlagged(ctx, asset); unread {
		return reserveUSDRead{series: reserveUSDSeries{reason: exclusionPriceReadFailed, degraded: true}}
	} else if flagged {
		return reserveUSDRead{series: reserveUSDSeries{reason: exclusionPriceWithheld}, verdict: true}
	}
	out := s.walkUSDRead(ctx, asset, q, budget)
	if out.series.reason == backstopUSDUnpriced {
		// A feed keyed by the wrapper's own contract id still prices it.
		if raw, err := canonical.ParseAsset(assetC); err == nil && !raw.Equal(asset) {
			if alt := s.walkUSDRead(ctx, raw, q, budget); alt.series.reason == "" {
				out = alt
			}
		}
	}
	out.series.dec = sc.dec
	return out
}

// reserveUSDIdentity is the asset a reserve's USD series is read under, or why the price data gives it none. An unread
// scale is a failed read like a failed price read, never decimals_unknown.
func (s *Server) reserveUSDIdentity(ctx context.Context, assetC string, sc reserveScale) (asset canonical.Asset, reason string, degraded bool) {
	if !sc.known && !sc.unread {
		return canonical.Asset{}, backstopUSDDecimals, false
	}
	asset, ok, unread := s.reserveTokenAsset(ctx, assetC)
	switch {
	case unread || s.History == nil:
		return asset, exclusionPriceReadFailed, true
	case !ok:
		return asset, backstopUSDUnpriced, false
	case sc.unread:
		return asset, exclusionPriceReadFailed, true
	case aggregate.ResolveDecimals(s.NonstandardDecimals, asset) != sc.dec:
		obs.NonstandardDecimalsLockstepMismatchTotal.WithLabelValues("lending_history", "").Inc()
		return asset, backstopUSDScaleMismatch, false
	}
	return asset, "", false
}

// walkUSDRead is one bounded read of asset's USD series: the window plus the price-age bound before it, ending at the bucket
// holding q.To.
func (s *Server) walkUSDRead(ctx context.Context, asset canonical.Asset, q clickhouse.LendingHistoryQuery, budget time.Duration) reserveUSDRead {
	pair, err := canonical.NewPair(asset, defaultPriceQuote)
	if err != nil {
		return reserveUSDRead{series: reserveUSDSeries{reason: backstopUSDUnpriced}}
	}
	gran, step := lendingHistoryGran(q)
	maxAge := lendingUSDPriceMaxAge(q)
	read := boundedChartRead(s.chartVWAPReader(gran), q.To.Add(step))
	pts, res, err := s.chartSeriesPoints(ctx, pair, chartWindow{from: q.From.Add(-maxAge), gran: gran, budget: budget}, read)
	if reason, degraded, verdict := usdWalkRefusal(pts, res, err); reason != "" {
		return reserveUSDRead{series: reserveUSDSeries{reason: reason, degraded: degraded}, verdict: verdict}
	}
	if reason, degraded := s.usdLatestThinRefusal(ctx, pair, res); reason != "" {
		return reserveUSDRead{series: reserveUSDSeries{reason: reason, degraded: degraded}, verdict: !degraded}
	}
	out := reserveUSDSeries{
		points: pts, maxAge: maxAge, step: step, proxied: res.proxied, degraded: res.degraded, withheld: !res.withheldFrom.IsZero(),
	}
	if len(res.crossLegs) > 0 {
		out.basis = priceBasisTransitive
	}
	for at, book := range res.bookAt {
		if !book.Quote.Equal(defaultPriceQuote) {
			if out.stableAt == nil {
				out.stableAt = map[time.Time]bool{}
			}
			out.stableAt[at.UTC()] = true
		}
	}
	return reserveUSDRead{series: out}
}

// usdWalkRefusal is why a walked series values nothing; verdict marks an operator withholding, while an unread input is
// degraded, not a verdict.
func usdWalkRefusal(pts []HistoryPoint, res chartWalkResult, err error) (reason string, degraded, verdict bool) {
	switch {
	case priceWithheldUnread(err):
		return exclusionPriceReadFailed, true, false
	case errors.Is(err, ErrPriceWithheld):
		return exclusionPriceWithheld, false, true
	case err != nil:
		return exclusionPriceReadFailed, true, false
	case len(pts) == 0:
		return backstopUSDUnpriced, res.degraded, false
	}
	return "", false, false
}

// usdLatestThinRefusal is the market-cap chart's thin-book guard on a walked series, so history never values a token the
// live reserve page withholds.
func (s *Server) usdLatestThinRefusal(ctx context.Context, pair canonical.Pair, res chartWalkResult) (string, bool) {
	thin, unmeasured := s.chartLatestThin(ctx, pair, res)
	switch {
	case thin:
		return exclusionPriceWithheld, false
	case unmeasured:
		return exclusionPriceReadFailed, true
	}
	return "", false
}

func lendingHistoryGran(q clickhouse.LendingHistoryQuery) (string, time.Duration) {
	if q.Daily {
		return "1d", 24 * time.Hour
	}
	return "1h", time.Hour
}

// boundedChartRead stops every read of a walk at ceiling, so a past window never scans to now.
func boundedChartRead(read chartRead, ceiling time.Time) chartRead {
	return func(ctx context.Context, p canonical.Pair, from, to time.Time, limit int) ([]HistoryPoint, error) {
		if to.IsZero() || to.After(ceiling) {
			to = ceiling
		}
		if !from.Before(to) {
			return nil, nil
		}
		return read(ctx, p, from, to, limit)
	}
}

// usdFlags folds a series' read outcome into the response flags.
func (u reserveUSDSeries) usdFlags(f Flags) Flags {
	f.Stale = f.Stale || u.degraded
	f.Degraded = f.Degraded || u.reason == exclusionPriceReadFailed
	f.Triangulated = f.Triangulated || u.proxied
	return markFillsWithheld(f, u.withheld)
}

// valueReserveHistory sets the decimals and each point's USD values, a point standing for the bucket it falls in (the seed
// for the first); one decimals read backs both, so a null decimals never sits beside a valued point.
func (s *Server) valueReserveHistory(ctx context.Context, view *LendingReserveHistoryView, h clickhouse.LendingReserveHistory, q clickhouse.LendingHistoryQuery, f Flags) Flags {
	view.PriceMaxAgeHours = int(lendingUSDPriceMaxAge(q) / time.Hour)
	last, ok := lastReserveState(h.Points)
	if !ok {
		return f
	}
	sc := s.reserveDecimals(ctx, last)
	view.Decimals = sc.wire()
	u := s.reserveUSDSeriesFor(ctx, view.Asset, sc, q)
	_, step := lendingHistoryGran(q)
	for i, p := range h.Points {
		at := p.CloseTime
		if at.Before(q.From) {
			at = q.From
		}
		v := usdValuation{reason: lendingUSDArchived}
		if !p.Removed {
			v = u.value(p.State, at.UTC().Truncate(step))
		}
		view.Points[i].LendingHistoryUSD = v.wire()
		if v.supplied == nil {
			view.USDUnvaluedPoints++
		}
	}
	return u.usdFlags(f)
}

// poolReserveSeries is one reserve's history and USD series in a pool history request.
type poolReserveSeries struct {
	asset string
	hist  clickhouse.LendingReserveHistory
	usd   reserveUSDSeries
	err   error
}

// handleLendingPoolHistory serves GET /v1/lending/pools/{pool}/history: the pool's TVL and borrowed USD at each bucket, every
// reserve's state at the bucket's close valued at that bucket's price.
func (s *Server) handleLendingPoolHistory(w http.ResponseWriter, r *http.Request) {
	pool := r.PathValue("pool")
	if !canonical.IsContractID(pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return
	}
	q, interval, ok := parseLendingHistoryQuery(w, r)
	if !ok {
		return
	}
	hr, ok := s.lendingHistoryReaderFor(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	protocol, _, ok := s.lendingHistoryLineage(ctx, w, r, pool)
	if !ok {
		return
	}
	assets, err := s.poolReserveAssets(ctx, pool, protocol)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "lending pool history assets", pool, err)
		return
	}
	series := make([]poolReserveSeries, len(assets))
	forEachBounded(s.logger, len(assets), readFanoutConcurrency, func(i int) {
		series[i] = s.readPoolReserveSeries(ctx, hr, pool, assets[i], q)
	})
	for _, rs := range series {
		if rs.err != nil && !errors.Is(rs.err, clickhouse.ErrLendingReserveNotFound) {
			s.writeLendingHistoryError(ctx, w, r, "LendingReserveHistory", pool, rs.err)
			return
		}
	}
	view := poolHistoryView(pool, protocol, interval, q, series, s.now())
	f := s.lendingHistoryFlags(ctx, view.lendingHistoryCoverageView)
	for _, rs := range series {
		f = rs.usd.usdFlags(f)
	}
	f = s.markReserveSetUnverified(ctx, &view, protocol, f)
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(time.Now().UTC()), Flags: f})
}

// markReserveSetUnverified makes every valued point a lower bound unless the projection the reserve list is read from is
// verified from genesis: the served tier names only reserves with a projected position, so an unverified one may miss some.
func (s *Server) markReserveSetUnverified(ctx context.Context, view *LendingPoolHistoryView, protocol string, f Flags) Flags {
	source := "blend"
	if protocol == blnt.SourceName {
		source = blnt.SourceName
	}
	var reason string
	switch s.projectionHistoryVerdicts(ctx, source)[source] {
	case historyVerified:
		return f
	case historyVerdictStale:
		reason, f.Stale = earningsReasonHistoryVerdictStale, true
	case historyVerdictUnread:
		reason, f.Degraded = earningsReasonHistoryVerdictUnread, true
	case historyUnverified:
		reason = earningsReasonHistoryUnverified
	}
	view.ReservesLowerBoundReason = strRef(reason)
	for i := range view.Points {
		view.Points[i].LowerBound = view.Points[i].TVLUSD != nil
	}
	return f
}

// poolReserveAssets lists the pool's reserve assets from its protocol's source.
func (s *Server) poolReserveAssets(ctx context.Context, pool, protocol string) ([]string, error) {
	if protocol != blnt.SourceName {
		return s.Lending.BlendPoolAssets(ctx, pool)
	}
	lr, ok := s.Lending.(blntLendingReader)
	if !ok {
		return nil, nil
	}
	return lr.BLNTPoolAssets(ctx, pool)
}

func (s *Server) readPoolReserveSeries(ctx context.Context, hr lendingHistoryReader, pool, asset string, q clickhouse.LendingHistoryQuery) poolReserveSeries {
	h, err := hr.LendingReserveHistory(ctx, pool, asset, q)
	if err != nil {
		return poolReserveSeries{asset: asset, err: err}
	}
	rs := poolReserveSeries{asset: asset, hist: h}
	if last, ok := lastReserveState(h.Points); ok {
		rs.usd = s.reserveUSDSeriesFor(ctx, asset, s.reserveDecimals(ctx, last), q)
	}
	return rs
}

// poolHistoryView builds the bucket grid from q.From to the earliest of q.To, now and the reserves' history coverage.
func poolHistoryView(pool, protocol, interval string, q clickhouse.LendingHistoryQuery, series []poolReserveSeries, now time.Time) LendingPoolHistoryView {
	v := LendingPoolHistoryView{
		Pool: pool, Protocol: nonEmptyPtr(protocol), Interval: interval, From: WireTime(q.From.UTC()), To: WireTime(q.To.UTC()),
		PriceMaxAgeHours: int(lendingUSDPriceMaxAge(q) / time.Hour), OmittedReserves: []string{}, Points: []LendingPoolHistoryPoint{},
	}
	var held []poolReserveSeries
	var cov clickhouse.LendingHistoryCoverage
	dropped := 0
	for _, rs := range series {
		if rs.err != nil {
			v.OmittedReserves = append(v.OmittedReserves, rs.asset)
			continue
		}
		if len(held) == 0 || rs.hist.Coverage.ThroughLedger < cov.ThroughLedger {
			cov = rs.hist.Coverage
		}
		dropped += rs.hist.Undecodable
		held = append(held, rs)
	}
	v.lendingHistoryCoverageView = coverageView(cov, dropped)
	_, step := lendingHistoryGran(q)
	end := q.To
	if now.Before(end) {
		end = now
	}
	if !cov.ThroughTime.IsZero() && cov.ThroughTime.Truncate(step).Add(step).Before(end) {
		end = cov.ThroughTime.Truncate(step).Add(step)
	}
	for b := q.From.UTC(); b.Before(end) && len(v.Points) < lendingHistoryMaxPoints; b = b.Add(step) {
		v.Points = append(v.Points, poolHistoryPoint(b, step, held, len(v.OmittedReserves) > 0))
	}
	return v
}

// poolHistoryPoint values each reserve's state at the close of bucket b at b's price. A reserve not yet listed is absent; one
// whose state entry is archived still holds tokens, so it is named unvalued.
func poolHistoryPoint(b time.Time, step time.Duration, held []poolReserveSeries, omitted bool) LendingPoolHistoryPoint {
	pt := LendingPoolHistoryPoint{
		T: WireTime(b), UnvaluedReserves: []string{}, PriceCarriedReserves: []string{}, Reserves: []LendingPoolHistoryReserve{},
	}
	var sup, bor []*big.Rat
	for _, rs := range held {
		p, ok := stateAtClose(rs.hist.Points, b.Add(step))
		if !ok {
			continue
		}
		v := usdValuation{reason: lendingUSDArchived}
		if !p.Removed {
			v = rs.usd.value(p.State, b)
		}
		pt.Reserves = append(pt.Reserves, LendingPoolHistoryReserve{Asset: rs.asset, LendingHistoryUSD: v.wire()})
		sup, bor = append(sup, v.supplied), append(bor, v.borrowed)
		switch {
		case v.supplied == nil:
			pt.UnvaluedReserves = append(pt.UnvaluedReserves, rs.asset)
		case v.carried:
			pt.PriceCarriedReserves = append(pt.PriceCarriedReserves, rs.asset)
		}
	}
	pt.TVLUSD, pt.LowerBound = reservesTVLUSD(sup)
	pt.BorrowedUSD, _ = reservesTVLUSD(bor)
	pt.LowerBound = pt.TVLUSD != nil && (pt.LowerBound || omitted)
	return pt
}

// stateAtClose is the last point recorded before end.
func stateAtClose(points []clickhouse.LendingReservePoint, end time.Time) (clickhouse.LendingReservePoint, bool) {
	i := sort.Search(len(points), func(i int) bool { return !points[i].CloseTime.Before(end) })
	if i == 0 {
		return clickhouse.LendingReservePoint{}, false
	}
	return points[i-1], true
}
