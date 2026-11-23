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

// lendingUSDPriceMaxAge is how far before a bucket its price may be: two days, as a market-cap day may carry its supply
// ([marketCapSupplyMaxCarry]), so a thin token that skipped a bucket is still valued. Past it the USD value is null.
const lendingUSDPriceMaxAge = 48 * time.Hour

// lendingUSDNoPriceInBound: the token has a USD series, but no bucket of it within [lendingUSDPriceMaxAge] before this one.
const lendingUSDNoPriceInBound = "no_price_within_bound"

// LendingHistoryUSD is an amount pair valued at the USD price of its own bucket; null with usd_reason when it is not valued.
type LendingHistoryUSD struct {
	SuppliedUSD *string `json:"supplied_usd"`
	BorrowedUSD *string `json:"borrowed_usd"`
	// PriceUSD is the per-whole-token price used and PriceAt the start of the price bucket it is the VWAP of.
	PriceUSD *string   `json:"price_usd"`
	PriceAt  *WireTime `json:"price_at"`
	// PriceBasis is "transitive" for a price crossed through XLM, omitted for a directly traded one.
	PriceBasis string  `json:"price_basis,omitempty"`
	USDReason  *string `json:"usd_reason"`
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
	OmittedReserves []string                  `json:"omitted_reserves"`
	Points          []LendingPoolHistoryPoint `json:"points"`
}

// LendingPoolHistoryPoint is the pool at the close of one bucket, each reserve's state valued at that bucket's price.
type LendingPoolHistoryPoint struct {
	T WireTime `json:"t"`
	// TVLUSD sums the valued reserves' supplied USD; null when none is valued.
	TVLUSD      *string `json:"tvl_usd"`
	BorrowedUSD *string `json:"borrowed_usd"`
	// LowerBound: TVLUSD leaves out the unvalued_reserves or the view's omitted_reserves.
	LowerBound       bool                        `json:"lower_bound"`
	UnvaluedReserves []string                    `json:"unvalued_reserves"`
	Reserves         []LendingPoolHistoryReserve `json:"reserves"`
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
	// reason is set when no bucket can be valued.
	reason                      string
	proxied, degraded, withheld bool
}

// usdValuation is one state's USD value; reason is set when supplied is nil.
type usdValuation struct {
	supplied, borrowed *big.Rat
	price, basis       string
	at                 time.Time
	reason             string
}

func (v usdValuation) wire() LendingHistoryUSD {
	if v.supplied == nil {
		return LendingHistoryUSD{USDReason: strRef(v.reason)}
	}
	at := WireTime(v.at.UTC())
	return LendingHistoryUSD{
		SuppliedUSD: strRef(v.supplied.FloatString(2)), BorrowedUSD: strRef(v.borrowed.FloatString(2)),
		PriceUSD: strRef(v.price), PriceAt: &at, PriceBasis: v.basis,
	}
}

// priceAt is the newest price bucket starting at or before bucket, if it is within [lendingUSDPriceMaxAge].
func (u reserveUSDSeries) priceAt(bucket time.Time) (HistoryPoint, bool) {
	i := sort.Search(len(u.points), func(i int) bool { return u.points[i].Bucket.After(bucket) })
	if i == 0 {
		return HistoryPoint{}, false
	}
	p := u.points[i-1]
	return p, bucket.Sub(p.Bucket) <= lendingUSDPriceMaxAge
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
	return usdValuation{supplied: sup, borrowed: bor, price: p.VWAP, basis: u.basis, at: p.Bucket}
}

// reserveUSDSeriesFor reads a reserve token's USD series for q through [Server.reservePriceUSD]'s identity and gates: the SAC
// unwrap, the reserve-decimals scale check and the issuer scam check, then the chart's alias and stablecoin walk.
func (s *Server) reserveUSDSeriesFor(ctx context.Context, assetC string, dec int, decKnown bool, q clickhouse.LendingHistoryQuery) reserveUSDSeries {
	asset, reason, degraded := s.reserveUSDAsset(ctx, assetC, dec, decKnown)
	if reason != "" {
		return reserveUSDSeries{reason: reason, degraded: degraded}
	}
	out := s.walkUSDSeries(ctx, asset, q)
	if out.reason == backstopUSDUnpriced {
		// A feed keyed by the wrapper's own contract id still prices it.
		if raw, err := canonical.ParseAsset(assetC); err == nil && !raw.Equal(asset) {
			if alt := s.walkUSDSeries(ctx, raw, q); alt.reason == "" {
				out = alt
			}
		}
	}
	out.dec = dec
	return out
}

// reserveUSDAsset is the asset a reserve's USD series is read under, or why it has none.
func (s *Server) reserveUSDAsset(ctx context.Context, assetC string, dec int, decKnown bool) (asset canonical.Asset, reason string, degraded bool) {
	if !decKnown {
		return canonical.Asset{}, backstopUSDDecimals, false
	}
	asset, ok, unread := s.reserveTokenAsset(ctx, assetC)
	switch {
	case unread || s.History == nil:
		return asset, exclusionPriceReadFailed, true
	case !ok:
		return asset, backstopUSDUnpriced, false
	case aggregate.ResolveDecimals(s.NonstandardDecimals, asset) != dec:
		obs.NonstandardDecimalsLockstepMismatchTotal.WithLabelValues("lending_history", "").Inc()
		return asset, backstopUSDScaleMismatch, false
	}
	if flagged, unread := s.reserveIssuerFlagged(ctx, asset); unread {
		return asset, exclusionPriceReadFailed, true
	} else if flagged {
		return asset, exclusionPriceWithheld, false
	}
	return asset, "", false
}

// walkUSDSeries is one bounded read of asset's USD series: the window plus the price-age bound before it, ending at the bucket
// holding q.To.
func (s *Server) walkUSDSeries(ctx context.Context, asset canonical.Asset, q clickhouse.LendingHistoryQuery) reserveUSDSeries {
	pair, err := canonical.NewPair(asset, defaultPriceQuote)
	if err != nil {
		return reserveUSDSeries{reason: backstopUSDUnpriced}
	}
	gran, step := lendingHistoryGran(q)
	read := boundedChartRead(s.chartVWAPReader(gran), q.To.Add(step))
	pts, res, err := s.chartSeriesPoints(ctx, pair, chartWindow{from: q.From.Add(-lendingUSDPriceMaxAge), gran: gran}, read)
	switch {
	case errors.Is(err, ErrPriceWithheld):
		return reserveUSDSeries{reason: exclusionPriceWithheld}
	case err != nil:
		return reserveUSDSeries{reason: exclusionPriceReadFailed, degraded: true}
	case len(pts) == 0:
		return reserveUSDSeries{reason: backstopUSDUnpriced, degraded: res.degraded}
	}
	out := reserveUSDSeries{points: pts, proxied: res.proxied, degraded: res.degraded, withheld: !res.withheldFrom.IsZero()}
	if len(res.crossLegs) > 0 {
		out.basis = priceBasisTransitive
	}
	return out
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

// valueReserveHistory sets each point's USD values, a point standing for the bucket it falls in (the seed for the first).
func (s *Server) valueReserveHistory(ctx context.Context, view *LendingReserveHistoryView, h clickhouse.LendingReserveHistory, q clickhouse.LendingHistoryQuery, f Flags) Flags {
	view.PriceMaxAgeHours = int(lendingUSDPriceMaxAge / time.Hour)
	last, ok := lastReserveState(h.Points)
	if !ok {
		return f
	}
	dec, known := s.reserveDecimals(ctx, last)
	u := s.reserveUSDSeriesFor(ctx, view.Asset, dec, known, q)
	_, step := lendingHistoryGran(q)
	for i, p := range h.Points {
		if p.Removed {
			continue
		}
		at := p.CloseTime
		if at.Before(q.From) {
			at = q.From
		}
		v := u.value(p.State, at.UTC().Truncate(step))
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
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(time.Now().UTC()), Flags: f})
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
		dec, known := s.reserveDecimals(ctx, last)
		rs.usd = s.reserveUSDSeriesFor(ctx, asset, dec, known, q)
	}
	return rs
}

// poolHistoryView builds the bucket grid from q.From to the earliest of q.To, now and the reserves' history coverage.
func poolHistoryView(pool, protocol, interval string, q clickhouse.LendingHistoryQuery, series []poolReserveSeries, now time.Time) LendingPoolHistoryView {
	v := LendingPoolHistoryView{
		Pool: pool, Protocol: nonEmptyPtr(protocol), Interval: interval, From: WireTime(q.From.UTC()), To: WireTime(q.To.UTC()),
		PriceMaxAgeHours: int(lendingUSDPriceMaxAge / time.Hour), OmittedReserves: []string{}, Points: []LendingPoolHistoryPoint{},
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

// poolHistoryPoint values each reserve's state at the close of bucket b at b's price; a reserve not yet listed or removed is absent.
func poolHistoryPoint(b time.Time, step time.Duration, held []poolReserveSeries, omitted bool) LendingPoolHistoryPoint {
	pt := LendingPoolHistoryPoint{T: WireTime(b), UnvaluedReserves: []string{}, Reserves: []LendingPoolHistoryReserve{}}
	var sup, bor []*big.Rat
	for _, rs := range held {
		p, ok := stateAtClose(rs.hist.Points, b.Add(step))
		if !ok || p.Removed {
			continue
		}
		v := rs.usd.value(p.State, b)
		pt.Reserves = append(pt.Reserves, LendingPoolHistoryReserve{Asset: rs.asset, LendingHistoryUSD: v.wire()})
		sup, bor = append(sup, v.supplied), append(bor, v.borrowed)
		if v.supplied == nil {
			pt.UnvaluedReserves = append(pt.UnvaluedReserves, rs.asset)
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
