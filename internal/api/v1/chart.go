package v1

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// ChartSeries is the /v1/chart wire shape (ADR-0020); `truncated` + `data_starts_at` flag a timeframe reaching past retention.
type ChartSeries struct {
	AssetID       string             `json:"asset_id"`
	Quote         string             `json:"quote"`
	Timeframe     string             `json:"timeframe"`
	Granularity   string             `json:"granularity"`
	PriceType     string             `json:"price_type"` // "vwap" | "twap" | "market_cap"
	Points        []HistoryPointWire `json:"points"`
	Truncated     bool               `json:"truncated"`                // true when the requested window starts before the earliest available data
	DataStartsAt  *WireTime          `json:"data_starts_at,omitempty"` // earliest bucket timestamp present in the result; only populated when Truncated
	RequestedFrom *WireTime          `json:"requested_from,omitempty"` // window start the consumer asked for; only populated when Truncated
	Discontinuous bool               `json:"discontinuous"`            // true when points skip at least one whole bucket between two served buckets
	GapStartsAt   *WireTime          `json:"gap_starts_at,omitempty"`  // last bucket before the WIDEST interior gap; only populated when Discontinuous
	GapEndsAt     *WireTime          `json:"gap_ends_at,omitempty"`    // first bucket after it; only populated when Discontinuous
	// MarketCapLowLiquidity: price_type=market_cap only. The series is
	// withheld (empty points) for the reason /v1/assets/{id} withholds
	// market_cap_usd and raises the same-named flag.
	MarketCapLowLiquidity bool `json:"market_cap_low_liquidity,omitempty"`
	// RowCapTruncated: the read hit historyMaxPoints, so Points holds the OLDEST slice and stops
	// short of the present. Only reachable for timeframe=all; DataEndsAt marks where it ends.
	RowCapTruncated bool      `json:"row_cap_truncated,omitempty"`
	DataEndsAt      *WireTime `json:"data_ends_at,omitempty"`
	// Credit names the vendor when every point of a fiat series comes from one licensed off-chain
	// feed; absent for on-chain series and for a cross or mix of feeds.
	Credit string `json:"credit,omitempty"`
}

// markDiscontinuity stamps the interior-gap signal onto a series about to be written: `points` has no holes, so a quiet market
// would otherwise look like a span we cannot answer. Threshold is the next bucket on the granularity's grid
// ([chartBucketStep]); only the WIDEST gap is reported, and an unknown grid emits nothing (a false gap claim is worse).
func (c *ChartSeries) markDiscontinuity() {
	for i := 1; i < len(c.Points); i++ {
		next := chartBucketStep(c.Points[i-1].T.Time(), c.Granularity)
		if next.IsZero() {
			return // unknown grain: no grid to measure a gap against
		}
		if !c.Points[i].T.Time().After(next) {
			continue // the very next bucket (or the same one)
		}
		gap := c.Points[i].T.Time().Sub(c.Points[i-1].T.Time())
		if c.Discontinuous && gap <= c.GapEndsAt.Time().Sub(c.GapStartsAt.Time()) {
			continue
		}
		from, to := c.Points[i-1].T, c.Points[i].T
		c.Discontinuous = true
		c.GapStartsAt = &from
		c.GapEndsAt = &to
	}
}

// chartBucketStep returns the START of the bucket after t on the CAGG `time_bucket` grid, zero if unknown. `1mo` advances by a
// CALENDAR month, unlike [chartGranularityGrace]'s fixed duration.
func chartBucketStep(t time.Time, gran string) time.Time {
	u := t.UTC()
	switch gran {
	case "1m":
		return u.Add(time.Minute)
	case "15m":
		return u.Add(15 * time.Minute)
	case "1h":
		return u.Add(time.Hour)
	case "4h":
		return u.Add(4 * time.Hour)
	case "1d":
		return u.AddDate(0, 0, 1)
	case "1w":
		return u.AddDate(0, 0, 7)
	case "1mo":
		return u.AddDate(0, 1, 0)
	default:
		return time.Time{}
	}
}

// writeChartJSON is the single write seam for /v1/chart series, so every shape carries its gap signal
// ([ChartSeries.markDiscontinuity]); the vwap path goes via [Server.writeChartSeries], which adds coverage.
func writeChartJSON(w http.ResponseWriter, series ChartSeries, flags Flags) {
	series.markDiscontinuity()
	writeJSON(w, series, flags)
}

// chartTimeframeSpec is a timeframe's window duration and default granularity; `all` has zero duration (no lower bound).
type chartTimeframeSpec struct {
	Duration       time.Duration
	DefaultGranule string
}

// chartTimeframes is the timeframe-to-spec table per ADR-0020; a new entry also needs the OpenAPI enum.
var chartTimeframes = map[string]chartTimeframeSpec{
	"1h":  {Duration: time.Hour, DefaultGranule: "1m"},
	"24h": {Duration: 24 * time.Hour, DefaultGranule: "15m"},
	"1w":  {Duration: 7 * 24 * time.Hour, DefaultGranule: "1h"},
	"1mo": {Duration: 30 * 24 * time.Hour, DefaultGranule: "4h"},
	"1y":  {Duration: 365 * 24 * time.Hour, DefaultGranule: "1d"},
	"all": {Duration: 0, DefaultGranule: "1d"},
}

// seriesWithheldForScam applies the directory-scam gate to a served price SERIES, writing the withheld problem and reporting true; `surface`
// is the low-cardinality metric label. Placement is load-bearing: before dispatchSpecialisedChart and from handleHistorySinceInception, and
// via [scamWithheld] it asks about BOTH legs. Raw /v1/history and /v1/observations trades stay visible; an AGGREGATED price claim does not.
func (s *Server) seriesWithheldForScam(w http.ResponseWriter, r *http.Request, pair canonical.Pair, surface string) bool {
	return s.writeIfScamWithheld(w, r, pair.Base, pair.Quote, surface)
}

// handleChart serves GET /v1/chart (defaults quote=USD, timeframe=24h, price_type=vwap): a CAGG series of CLOSED buckets
// (ADR-0015). twap and market_cap are dispatched in dispatchSpecialisedChart first.
func (s *Server) handleChart(w http.ResponseWriter, r *http.Request) {
	if s.History == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/history-unavailable",
			"History serving not configured", http.StatusServiceUnavailable,
			"this deployment has no HistoryReader wired — check binary configuration")
		return
	}

	pair, ok := parseChartPair(w, r)
	if !ok {
		return
	}

	if s.seriesWithheldForScam(w, r, pair, "chart") {
		return
	}

	tfRaw, tf, gran, priceType, ok := parseChartParams(w, r)
	if !ok {
		return
	}

	var from time.Time
	if tf.Duration > 0 {
		from = time.Now().Add(-tf.Duration).UTC()
	} else if !middleware.ChargeRateLimit(w, r, sinceInceptionCost(gran)) {
		// timeframe=all has no window to coarsen against, so its read is
		// priced by grain like /v1/history/since-inception.
		return
	}

	// Dispatch to specialised handlers when the request shape calls
	// for it; fall through to the default vwap-on-prices_1m path
	// when no specialisation matches.
	if s.dispatchSpecialisedChart(w, r, pair, tfRaw, gran, priceType, from) {
		return
	}

	// Serve the finest grain the window can carry ([chartFitGranularity]). `gran` is REPLACED
	// rather than shadowed so the read, retention grace, wire `granularity` and logs all describe
	// the series actually produced; an unknown grain passes through for the 400 below.
	gran = chartFitGranularity(chartVWAPGranularityLadder, gran, tf.Duration)

	// 8s ceiling on the chart query plus stablecoin fallback: the prices_* scan can take 5-10s
	// on a cold cache for long timeframes (1y at 1h is ~8,760 buckets).
	chartCtx, chartCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer chartCancel()
	points, walk, err := s.chartSeriesPoints(chartCtx, pair,
		chartWindow{from: from, gran: gran}, s.chartVWAPReader(gran))
	if errors.Is(err, ErrUnknownGranularity) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-granularity",
			"Invalid granularity", http.StatusBadRequest,
			// Enumerated from timescale.AllHistoryGranularities so the message cannot drift
			// from what Validate accepts.
			fmt.Sprintf("granularity must be one of: %s (got %q)", timescale.HistoryGranularityList(), gran))
		return
	}
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(chartCtx, err) {
			s.logger.Warn("HistoryPointsInRange deadline exceeded",
				"asset", pair.Base.String(), "quote", pair.Quote.String(),
				"timeframe", tfRaw, "granularity", gran)
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/chart-timeout",
				"Chart query timed out", http.StatusServiceUnavailable,
				"the underlying prices_1m / prices_5m / prices_1h scan didn't return in 8s; cache may still be warming. Retry in a few seconds.")
			return
		}
		s.logger.Error("HistoryPointsInRange failed",
			"err", err, "asset", pair.Base.String(), "quote", pair.Quote.String(),
			"timeframe", tfRaw, "granularity", gran)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	wire := make([]HistoryPointWire, len(points))
	for i, p := range points {
		wire[i] = historyWirePoint(p)
	}

	series := ChartSeries{
		AssetID:     pair.Base.String(),
		Quote:       pair.Quote.String(),
		Timeframe:   tfRaw,
		Granularity: gran,
		PriceType:   priceType,
		Points:      wire,
	}
	// Truncated when a bounded window's earliest bucket starts more than one granularity unit
	// after `from`: the difference between "the last 7 days are flat" and "this deployment only
	// has 7 days". `timeframe=all` never trips it; there a short result IS the full result.
	if !from.IsZero() && len(points) > 0 {
		if grace := chartGranularityGrace(gran); points[0].Bucket.Sub(from) > grace {
			startsAt := points[0].Bucket
			requested := from
			series.Truncated = true
			series.DataStartsAt = wireTimePtr(&startsAt)
			series.RequestedFrom = wireTimePtr(&requested)
		}
	}

	series.markRowCap(walk)

	s.writeChartSeries(w, r, pair, series, walk)
}

// dispatchSpecialisedChart routes market_cap series, fiat:fiat pairs (fx_quotes, not prices_1m)
// and price_type=twap to their handlers. It returns true when one took the request.
func (s *Server) dispatchSpecialisedChart(
	w http.ResponseWriter,
	r *http.Request,
	pair canonical.Pair,
	tfRaw, gran, priceType string,
	from time.Time,
) bool {
	if priceType == "market_cap" {
		s.handleChartMarketCap(w, r, pair, tfRaw, gran, from)
		return true
	}
	if pair.Base.Type == canonical.AssetFiat && pair.Quote.Type == canonical.AssetFiat {
		// Fiat:fiat pairs (incl. cross-fiat triangulation) are served from
		// fx_quotes for EVERY price_type — the daily reference rate IS the
		// time series, so a twap request on fiat/fiat returns the same fx
		// series (there is no sub-daily trade stream to time-weight).
		s.handleChartFiat(w, r, pair, tfRaw, gran, priceType, from)
		return true
	}
	if priceType == "twap" {
		s.handleChartTWAP(w, r, pair, tfRaw, gran, from)
		return true
	}
	return false
}

// fiatChartGranularity snaps any granularity to the one fx_quotes serves (daily); the SNAPPED value is stamped so
// [markDiscontinuity] and truncation measure the grid served.
func fiatChartGranularity(string) string {
	return string(timescale.Granularity1d)
}

// twapChartGranularity snaps to the two TWAP CAGG grains (sub-daily to 1h, else 1d); handleChartTWAP reports the snapped grain.
func twapChartGranularity(gran string) string {
	switch gran {
	case "1d", "1w", "1mo":
		return "1d"
	default: // 1m, 15m, 1h, 4h and any unknown → the finer TWAP grain
		return "1h"
	}
}

// ─── granularity fitting ────────────────────────────────────────

// chartVWAPGranularityLadder is the default path's coarsening ladder, finest first. It IS
// [timescale.AllHistoryGranularities] so two copies of the served set cannot drift.
var chartVWAPGranularityLadder = timescale.AllHistoryGranularities

// chartTWAPGranularityLadder is deliberately NOT the served set: only twap_1h and twap_1d exist,
// so coarsening onto `4h` or past `1d` would name a view that does not exist.
var chartTWAPGranularityLadder = []timescale.HistoryGranularity{
	timescale.Granularity1h, timescale.Granularity1d,
}

// chartGranularityFits reports whether the `gran` grid over `window` has at most [historyMaxPoints] points; an unknown grain
// "fits" so [ErrUnknownGranularity] still becomes the 400.
func chartGranularityFits(gran string, window time.Duration) bool {
	width := timescale.HistoryGranularity(gran).BucketDuration()
	if width <= 0 {
		return true
	}
	// CEILING, not floor: 50,000.4 bucket widths can touch 50,001 grid points.
	return (window+width-1)/width <= historyMaxPoints
}

// chartFitGranularity is the grain a window is SERVED at: `gran` if it fits, else the finest COARSER rung of `ladder` that
// does. The reader truncates the EARLIEST buckets, so an unfitted 1y at 1m would be a 200 covering 36 of 365 days labelled
// "1m". Depends on the REQUEST alone: `window <= 0` or an exhausted ladder returns `gran` untouched.
func chartFitGranularity(ladder []timescale.HistoryGranularity, gran string, window time.Duration) string {
	if window <= 0 || chartGranularityFits(gran, window) {
		return gran
	}
	coarser := false
	for _, g := range ladder {
		if !coarser {
			coarser = string(g) == gran
			continue
		}
		if chartGranularityFits(string(g), window) {
			return string(g)
		}
	}
	return gran
}

// handleChartTWAP serves price_type=twap for a non-fiat base from the twap_1h / twap_1d CAGGs, mirroring the VWAP path (closed
// buckets, stablecoin-USD proxy fallback) at the TWAP grain.
func (s *Server) handleChartTWAP(
	w http.ResponseWriter,
	r *http.Request,
	pair canonical.Pair,
	tfRaw, gran string,
	from time.Time,
) {
	// Snap onto a TWAP grain, then coarsen if that grid still outruns one response. Today a guard
	// only (1y is 8,760 buckets at 1h), kept because chartTimeframes is meant to be extended.
	twapGran := chartFitGranularity(chartTWAPGranularityLadder,
		twapChartGranularity(gran), chartTimeframes[tfRaw].Duration)

	// 8s ceiling covering the CAGG scan + the proxy fallback retry,
	// matching the VWAP path.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	read := func(rc context.Context, p canonical.Pair, lo, hi time.Time, limit int) ([]HistoryPoint, error) {
		return s.History.TWAPPointsInRange(rc, p, twapGran, lo, hi, limit)
	}

	points, walk, err := s.chartSeriesPoints(ctx, pair,
		chartWindow{from: from, gran: twapGran}, read)
	if errors.Is(err, ErrUnknownGranularity) {
		// twapChartGranularity only ever emits 1h / 1d, both of which have
		// a CAGG — this arm guards a future grain change, not user input.
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-granularity",
			"Invalid granularity", http.StatusBadRequest,
			"price_type=twap serves 1h and 1d resolutions only")
		return
	}
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(ctx, err) {
			s.logger.Warn("TWAPPointsInRange deadline exceeded",
				"asset", pair.Base.String(), "quote", pair.Quote.String(),
				"timeframe", tfRaw, "granularity", twapGran)
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/chart-timeout",
				"Chart query timed out", http.StatusServiceUnavailable,
				"the underlying twap_1h / twap_1d scan didn't return in 8s; cache may still be warming. Retry in a few seconds.")
			return
		}
		s.logger.Error("TWAPPointsInRange failed",
			"err", err, "asset", pair.Base.String(), "quote", pair.Quote.String(),
			"timeframe", tfRaw, "granularity", twapGran)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	wire := make([]HistoryPointWire, len(points))
	for i, p := range points {
		wire[i] = historyWirePoint(p)
	}

	series := ChartSeries{
		AssetID:     pair.Base.String(),
		Quote:       pair.Quote.String(),
		Timeframe:   tfRaw,
		Granularity: twapGran, // the grain actually served (snapped)
		PriceType:   "twap",
		Points:      wire,
	}
	if !from.IsZero() && len(points) > 0 {
		if grace := chartGranularityGrace(twapGran); points[0].Bucket.Sub(from) > grace {
			startsAt := points[0].Bucket
			requested := from
			series.Truncated = true
			series.DataStartsAt = wireTimePtr(&startsAt)
			series.RequestedFrom = wireTimePtr(&requested)
		}
	}
	series.markRowCap(walk)
	writeChartJSON(w, series, Flags{Triangulated: walk.proxied, Stale: walk.degraded, Degraded: walk.degraded})
}

// handleChartFiat serves fiat:fiat pairs from fx_quotes, one row per ticker per UTC day. The reader gives CCY-per-USD:
// fiat:CCY/fiat:USD uses InverseUSD, fiat:USD/fiat:CCY RateUSD, and a cross goes to [Server.handleChartFiatCross].
func (s *Server) handleChartFiat(
	w http.ResponseWriter,
	r *http.Request,
	pair canonical.Pair,
	tfRaw, gran, priceType string,
	from time.Time,
) {
	gran = fiatChartGranularity(gran)
	series := ChartSeries{
		AssetID:     pair.Base.String(),
		Quote:       pair.Quote.String(),
		Timeframe:   tfRaw,
		Granularity: gran, // the grain actually served (snapped)
		PriceType:   priceType,
		Points:      []HistoryPointWire{},
	}

	if s.FXHistory == nil {
		writeChartJSON(w, series, Flags{})
		return
	}

	// Identify the non-USD ticker + which side it's on.
	var ticker string
	var useInverse bool
	switch {
	case pair.Base.Code == "USD" && pair.Quote.Code != "USD":
		ticker, useInverse = pair.Quote.Code, false
	case pair.Quote.Code == "USD" && pair.Base.Code != "USD":
		ticker, useInverse = pair.Base.Code, true
	default:
		// Cross-fiat (e.g. EUR/JPY) — triangulate both legs vs USD.
		// (USD/USD can't reach here: identity pairs are rejected in
		// parseChartPair.)
		s.handleChartFiatCross(w, r, pair, series, gran, from)
		return
	}

	// Default window: trailing 25y (ECB inception) when timeframe=all.
	to := time.Now().UTC().Truncate(24 * time.Hour)
	queryFrom := from
	if queryFrom.IsZero() {
		queryFrom = to.AddDate(-25, 0, 0) // ECB inception
	}

	fxCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	points, err := s.FXHistory.ListFXHistory(fxCtx, ticker, queryFrom, to)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(fxCtx, err) {
			s.writeChartTimeout(w, r, "ListFXHistory", ticker)
			return
		}
		s.logger.Warn("chart fiat fx_quotes fetch failed",
			"ticker", ticker, "err", err)
		writeChartJSON(w, series, Flags{Stale: true, Degraded: true})
		return
	}

	wire := make([]HistoryPointWire, 0, len(points))
	for _, p := range points {
		r, ok := p.rateUSDRat()
		if useInverse {
			r, ok = p.inverseUSDRat()
		}
		if !ok {
			continue
		}
		wire = append(wire, HistoryPointWire{
			T: WireTime(p.Bucket),
			P: ratToDecimal(r, ohlcPriceDigits),
			// FX rates have no volume — omit v_usd entirely.
		})
	}
	series.Points = wire
	series.Credit = fxSeriesCredit(points)

	// Retention-truncation signal — same shape as the crypto path.
	if !from.IsZero() && len(wire) > 0 {
		if grace := chartGranularityGrace(gran); wire[0].T.Time().Sub(from) > grace {
			startsAt := wire[0].T
			requested := from
			series.Truncated = true
			series.DataStartsAt = &startsAt
			series.RequestedFrom = wireTimePtr(&requested)
		}
	}

	writeChartJSON(w, series, Flags{})
}

// handleChartFiatCross serves non-USD fiat crosses as rate_usd[quote] / rate_usd[base] (as /v1/price's tryFiatCrossRate). A day
// missing either leg is skipped, never forward-filled; division is big.Rat (ADR-0003); flags.triangulated is set.
func (s *Server) handleChartFiatCross(
	w http.ResponseWriter,
	r *http.Request,
	pair canonical.Pair,
	series ChartSeries,
	gran string,
	from time.Time,
) {
	// Same default window as the direct fiat path: trailing 25y when
	// timeframe=all (ECB inception).
	to := time.Now().UTC().Truncate(24 * time.Hour)
	queryFrom := from
	if queryFrom.IsZero() {
		queryFrom = to.AddDate(-25, 0, 0)
	}

	fxCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	basePts, err := s.FXHistory.ListFXHistory(fxCtx, pair.Base.Code, queryFrom, to)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(fxCtx, err) {
			s.writeChartTimeout(w, r, "ListFXHistory", pair.Base.Code)
			return
		}
		s.logger.Warn("chart fiat-cross fx_quotes fetch failed",
			"ticker", pair.Base.Code, "err", err)
		writeChartJSON(w, series, Flags{Stale: true, Degraded: true})
		return
	}
	quotePts, err := s.FXHistory.ListFXHistory(fxCtx, pair.Quote.Code, queryFrom, to)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(fxCtx, err) {
			s.writeChartTimeout(w, r, "ListFXHistory", pair.Quote.Code)
			return
		}
		s.logger.Warn("chart fiat-cross fx_quotes fetch failed",
			"ticker", pair.Quote.Code, "err", err)
		writeChartJSON(w, series, Flags{Stale: true, Degraded: true})
		return
	}

	wire := crossFiatChartPoints(basePts, quotePts)
	series.Points = wire
	series.Credit = fxSeriesCredit(append(append([]FXQuotePoint{}, basePts...), quotePts...))

	// Retention-truncation signal — same shape as the direct path.
	if !from.IsZero() && len(wire) > 0 {
		if grace := chartGranularityGrace(gran); wire[0].T.Time().Sub(from) > grace {
			startsAt := wire[0].T
			requested := from
			series.Truncated = true
			series.DataStartsAt = &startsAt
			series.RequestedFrom = wireTimePtr(&requested)
		}
	}
	writeChartJSON(w, series, Flags{Triangulated: len(wire) > 0})
}

// crossFiatChartPoints merges two ascending daily USD-leg series on equal buckets and emits
// rate_usd[quote]/rate_usd[base] per shared day, dividing the exact NUMERIC rates in big.Rat.
func crossFiatChartPoints(basePts, quotePts []FXQuotePoint) []HistoryPointWire {
	n := len(basePts)
	if len(quotePts) < n {
		n = len(quotePts)
	}
	wire := make([]HistoryPointWire, 0, n)
	i, j := 0, 0
	for i < len(basePts) && j < len(quotePts) {
		b, q := basePts[i], quotePts[j]
		switch {
		case b.Bucket.Before(q.Bucket):
			i++
		case q.Bucket.Before(b.Bucket):
			j++
		default:
			i++
			j++
			br, bok := b.rateUSDRat()
			qr, qok := q.rateUSDRat()
			if !bok || !qok {
				continue
			}
			cross := new(big.Rat).Quo(qr, br)
			wire = append(wire, HistoryPointWire{
				T: WireTime(b.Bucket),
				P: ratToDecimal(cross, ohlcPriceDigits),
				// FX rates have no volume — omit v_usd entirely.
			})
		}
	}
	return wire
}

// chartGranularityGrace is the first-bucket gap above which a response counts as retention-truncated: one granularity period;
// unknown grains get 1 day to avoid false positives.
func chartGranularityGrace(gran string) time.Duration {
	switch gran {
	case "1m":
		return time.Minute
	case "15m":
		return 15 * time.Minute
	case "1h":
		return time.Hour
	case "4h":
		return 4 * time.Hour
	case "1d":
		return 24 * time.Hour
	case "1w":
		return 7 * 24 * time.Hour
	case "1mo":
		return 30 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// chartBucketMerge accumulates a series bucket by bucket: the FIRST source offered a bucket owns it, and later sources are suppressed for
// that bucket only ("The fiat quote leg, per bucket" in docs/architecture/aggregation-plan.md). Once-per-response first-hit would make the
// source set depend on the WINDOW. Sources are never blended within a bucket, so no VWAP is published that no venue set produced.
type chartBucketMerge struct {
	byBucket map[time.Time]HistoryPoint
}

func newChartBucketMerge() *chartBucketMerge {
	return &chartBucketMerge{byBucket: make(map[time.Time]HistoryPoint)}
}

// empty reports whether no bucket is held. Buckets are keyed in UTC because [time.Time] compares location as well as instant.
func (m *chartBucketMerge) empty() bool { return len(m.byBucket) == 0 }

func (m *chartBucketMerge) add(points []HistoryPoint) int {
	claimed := 0
	for _, p := range points {
		k := p.Bucket.UTC()
		if _, taken := m.byBucket[k]; taken {
			continue
		}
		m.byBucket[k] = p
		claimed++
	}
	return claimed
}

// earliest is the oldest claimed bucket (zero when none); [chartWindow.covered] anchors its grid walk on it. Merged series must
// be ascending ([ChartSeries.markDiscontinuity] assumes it).
func (m *chartBucketMerge) earliest() time.Time {
	var first time.Time
	for b := range m.byBucket {
		if first.IsZero() || b.Before(first) {
			first = b
		}
	}
	return first
}

func (m *chartBucketMerge) series() []HistoryPoint {
	if len(m.byBucket) == 0 {
		return nil
	}
	out := make([]HistoryPoint, 0, len(m.byBucket))
	for _, p := range m.byBucket {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bucket.Before(out[j].Bucket) })
	return out
}

// chartWindow is the request a source walk must cover: the read's lower bound and bucket grid. A zero `from` is unbounded, so
// coverage is never established and the walk runs its whole list.
type chartWindow struct {
	from time.Time
	gran string
}

// covered reports whether `m` already holds EVERY bucket the reader can return, so the walk may stop identically to the full
// walk (the store returns only CLOSED grid-point buckets at or after `from`). Two UNENFORCED preconditions: every claimed
// bucket is a grid point (reads go to `prices_<gran>` / `twap_<gran>`), and the reader's closed-bucket clock is not ahead of
// the handler's (one UTC host, docs/architecture/ha-plan.md §3.6). A failure truncates silently, with no `flags.stale`.
func (w chartWindow) covered(m *chartBucketMerge, now time.Time) bool {
	if w.from.IsZero() || m.empty() {
		return false
	}
	first := m.earliest()
	prev := chartBucketPrev(first, w.gran)
	if prev.IsZero() || !prev.Before(w.from) {
		// Unknown grain, or a whole readable bucket of the window sits
		// before the earliest hit — a later source could still fill it.
		return false
	}
	n, ok := chartClosedBucketCount(first, now, w.gran)
	return ok && n > 0 && len(m.byBucket) >= n
}

// chartClosedBucketCount is how many grid buckets from `first` have CLOSED by `now`: the exact
// row count the reader could return for a window starting there. ok=false for an unknown grain.
func chartClosedBucketCount(first, now time.Time, gran string) (int, bool) {
	if d, fixed := chartFixedBucketWidth(gran); fixed {
		if !first.Add(d).After(now) {
			return int(now.Sub(first) / d), true
		}
		return 0, true
	}
	if chartBucketStep(first, gran).IsZero() {
		return 0, false
	}
	n := 0
	for b := first; !chartBucketStep(b, gran).After(now); b = chartBucketStep(b, gran) {
		n++
	}
	return n, true
}

// chartFixedBucketWidth returns a granularity's constant bucket width, and whether it HAS one.
// `1mo` does not (calendar months, 28 to 31 days); only [chartBucketStep] can walk it.
func chartFixedBucketWidth(gran string) (time.Duration, bool) {
	switch gran {
	case "1m":
		return time.Minute, true
	case "15m":
		return 15 * time.Minute, true
	case "1h":
		return time.Hour, true
	case "4h":
		return 4 * time.Hour, true
	case "1d":
		return 24 * time.Hour, true
	case "1w":
		return 7 * 24 * time.Hour, true
	default:
		return 0, false
	}
}

// chartBucketPrev is [chartBucketStep] backwards; safe for `1mo` since a monthly bucket start is midnight on the 1st.
func chartBucketPrev(t time.Time, gran string) time.Time {
	u := t.UTC()
	if d, fixed := chartFixedBucketWidth(gran); fixed {
		return u.Add(-d)
	}
	if gran == "1mo" {
		return u.AddDate(0, -1, 0)
	}
	return time.Time{}
}

// chartWalkBudget bounds reads a walk makes ONCE IT HOLDS A SERIES; reads on an empty merge keep the handler's 8s ceiling. The
// walk fans out up to 24 uncached CAGG scans, so unbounded a slow constituent at 1y/1m turns a 200 into a `503 chart-timeout`.
// A walk stopped here serves what it merged and stamps flags.stale.
const chartWalkBudget = 2 * time.Second

// chartWalkResult is what one source walk produced, beyond the points.
type chartWalkResult struct {
	// proxied is true when a served bucket came from a source whose
	// QUOTE leg is a proxy for the requested one → flags.triangulated.
	proxied bool
	// degraded is true when the walk stopped early (a read failed or the budget ran out), so the
	// series may be shorter than a full walk's → flags.stale.
	degraded bool
	// cappedAt is the last bucket of the earliest source read that hit
	// historyMaxPoints on an unbounded window; the served series is
	// incomplete after it. Zero when no read was capped.
	cappedAt time.Time
}

// noteCap records a source read that filled the row cap, judged on the
// raw read before any merge so a union of uncapped sources never trips it.
func (r *chartWalkResult) noteCap(win chartWindow, points []HistoryPoint, limit int) {
	if !win.from.IsZero() || limit != historyMaxPoints || len(points) != limit {
		return
	}
	r.mergeCap(points[len(points)-1].Bucket)
}

func (r *chartWalkResult) mergeCap(at time.Time) {
	if !at.IsZero() && (r.cappedAt.IsZero() || at.Before(r.cappedAt)) {
		r.cappedAt = at
	}
}

// markRowCap stamps the row-cap truncation signal onto a series.
func (s *ChartSeries) markRowCap(res chartWalkResult) {
	if res.cappedAt.IsZero() {
		return
	}
	s.RowCapTruncated = true
	end := WireTime(res.cappedAt)
	s.DataEndsAt = &end
}

// chartRead fetches up to `limit` of one source pair's closed buckets in
// [from, to), oldest first; a zero bound is unbounded on that side.
type chartRead func(ctx context.Context, p canonical.Pair, from, to time.Time, limit int) ([]HistoryPoint, error)

// chartSpan is a half-open bucket range [from, to) of a [chartRead].
type chartSpan struct{ from, to time.Time }

// missingSpans returns the window's readable buckets `m` does not hold, as ranges on the [chartWindow.covered] grid, so later
// reads scan only the gaps. nil when unbounded below or the grain is unknown.
func (w chartWindow) missingSpans(m *chartBucketMerge, now time.Time) []chartSpan {
	if w.from.IsZero() || m.empty() || chartBucketStep(m.earliest(), w.gran).IsZero() {
		return nil
	}
	b := m.earliest()
	for p := chartBucketPrev(b, w.gran); !p.Before(w.from); p = chartBucketPrev(p, w.gran) {
		b = p
	}
	for b.Before(w.from) {
		b = chartBucketStep(b, w.gran)
	}
	start := b
	var runs []chartSpan
	for next := chartBucketStep(b, w.gran); !next.After(now); b, next = next, chartBucketStep(next, w.gran) {
		if _, held := m.byBucket[b]; held {
			continue
		}
		if n := len(runs); n > 0 && runs[n-1].to.Equal(b) {
			runs[n-1].to = next
			continue
		}
		runs = append(runs, chartSpan{from: b, to: next})
	}
	if len(runs) > 0 && runs[0].from.Equal(start) {
		runs[0].from = w.from // the store reads buckets at or after the window's own bound
	}
	return coalesceChartSpans(runs)
}

// coalesceChartSpans bridges ascending disjoint runs across their narrowest gaps while the bridged width stays within the runs'
// own: at most twice the missing width in at most len(runs) reads.
func coalesceChartSpans(runs []chartSpan) []chartSpan {
	if len(runs) < 2 {
		return runs
	}
	var slack time.Duration
	for _, r := range runs {
		slack += r.to.Sub(r.from)
	}
	gap := func(i int) time.Duration { return runs[i+1].from.Sub(runs[i].to) }
	order := make([]int, len(runs)-1) // gap i sits between runs[i] and runs[i+1]
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return gap(order[a]) < gap(order[b]) })
	bridged := make([]bool, len(order))
	for _, i := range order {
		if slack -= gap(i); slack < 0 {
			break
		}
		bridged[i] = true
	}
	out := []chartSpan{runs[0]}
	for i, r := range runs[1:] {
		if bridged[i] {
			out[len(out)-1].to = r.to
			continue
		}
		out = append(out, r)
	}
	return out
}

// chartWalk is the state one source walk shares across its reads.
type chartWalk struct {
	s     *Server
	pair  canonical.Pair
	win   chartWindow
	read  chartRead
	merge *chartBucketMerge
	res   chartWalkResult
	spent time.Duration // time spent on reads taken after the merge filled
}

func (s *Server) newChartWalk(pair canonical.Pair, win chartWindow, read chartRead) *chartWalk {
	return &chartWalk{s: s, pair: pair, win: win, read: read, merge: newChartBucketMerge()}
}

// chartSourceClass splits the walk's ERROR handling. An ALIAS source is the requested pair in another spelling, so its failure
// IS the answer; a PROXY source (peg or backer quote) failure is skipped and never the answer.
type chartSourceClass int

const (
	chartSourceAlias chartSourceClass = iota
	chartSourceProxy
)

// step reads one source into the merge; false means stop, and an error is returned only for a read whose failure is the answer. Two axes,
// not to be conflated: BUDGET follows merge state (empty merge keeps the full 8s; once a series is held [chartWalkBudget] bounds fills),
// ERRORS follow source CLASS (an early proxy failure must not become a `503` instead of a `200`); a proxy failure CONTINUES.
func (w *chartWalk) step(ctx context.Context, sp canonical.Pair, class chartSourceClass) (bool, error) {
	if w.merge.empty() {
		// Nothing merged yet: this read can still be the answer, so it keeps the handler's
		// ceiling and, for an alias source, its error handling.
		points, err := w.read(ctx, sp, w.win.from, time.Time{}, historyMaxPoints)
		if err != nil {
			if class == chartSourceAlias {
				return false, err
			}
			w.res.degraded = true
			return true, nil //nolint:nilerr // a proxy failure is never the answer — see above
		}
		w.res.noteCap(w.win, points, historyMaxPoints)
		w.claim(sp, points)
		return true, nil
	}
	now := time.Now().UTC()
	if w.win.covered(w.merge, now) {
		return false, nil // every readable bucket is already claimed
	}
	spans := w.win.missingSpans(w.merge, now)
	if len(spans) == 0 {
		spans = []chartSpan{{from: w.win.from}}
	}
	return w.fill(ctx, sp, spans), nil
}

// fill reads one source over `spans` inside [chartWalkBudget], reporting whether the walk may continue; flags.stale is set
// exactly when a read failed or the budget cut the walk short.
func (w *chartWalk) fill(ctx context.Context, sp canonical.Pair, spans []chartSpan) bool {
	if len(spans) > 1 {
		// A one-bucket read over the spans' hull shows whether this source
		// holds anything they could claim, so an empty one costs one read.
		probe, cont, ok := w.readSpan(ctx, sp, chartSpan{from: spans[0].from, to: spans[len(spans)-1].to}, 1)
		if !ok || len(probe) == 0 {
			return cont
		}
	}
	for _, span := range spans {
		points, cont, ok := w.readSpan(ctx, sp, span, historyMaxPoints)
		if !ok {
			return cont
		}
		w.res.noteCap(w.win, points, historyMaxPoints)
		w.claim(sp, points)
	}
	return true
}

// readSpan is one budgeted read of [chartWalk.fill]. ok is false when the
// read failed or the budget is spent, and cont whether the walk may go on.
func (w *chartWalk) readSpan(
	ctx context.Context, sp canonical.Pair, span chartSpan, limit int,
) (points []HistoryPoint, cont, ok bool) {
	remaining := chartWalkBudget - w.spent
	if remaining <= 0 {
		w.res.degraded = true
		return nil, false, false
	}
	rctx, cancel := context.WithTimeout(ctx, remaining)
	started := time.Now()
	points, err := w.read(rctx, sp, span.from, span.to, limit)
	cancel()
	w.spent += time.Since(started)
	if err != nil {
		// The error is CARRIED as flags.stale, not returned: returning it would discard every
		// bucket already merged and answer 503, so one slow proxy would destroy a complete
		// series. A LATER spelling can still hold buckets this one lacks, so the walk
		// continues; the budget check above ends it when the budget was the failure.
		w.res.degraded = true
		return nil, true, false
	}
	return points, true, true
}

// claim normalises one source's points to true prices and offers them
// to the merge, recording whether the source's quote leg was a proxy.
func (w *chartWalk) claim(sp canonical.Pair, points []HistoryPoint) {
	points = w.s.adjustSourcePoints(sp, points)
	if w.merge.add(points) > 0 && !sp.Quote.Equal(w.pair.Quote) {
		w.res.proxied = true
	}
}

// adjustSourcePoints applies the nonstandard-decimals normalisation per SOURCE pair, not over the merged array: the
// 10^(baseDecimals - quoteDecimals) factor belongs to the pair the bucket was READ from.
func (s *Server) adjustSourcePoints(sp canonical.Pair, points []HistoryPoint) []HistoryPoint {
	return adjustHistoryPointPrices(points,
		aggregate.ResolveDecimals(s.NonstandardDecimals, sp.Base),
		aggregate.ResolveDecimals(s.NonstandardDecimals, sp.Quote))
}

// chartSeriesPoints is the one entry point for chart surfaces: observed markets ([Server.chartObservedPoints]), then only if
// those answered nothing the series derived through XLM ([Server.fiatSeriesThroughXLM]). Derived is a WHOLE-SERIES last resort,
// never a per-bucket filler (two kinds of number in one array).
func (s *Server) chartSeriesPoints(
	ctx context.Context,
	pair canonical.Pair,
	win chartWindow,
	read chartRead,
) ([]HistoryPoint, chartWalkResult, error) {
	points, res, err := s.chartObservedPoints(ctx, pair, win, read)
	if err != nil {
		return nil, chartWalkResult{}, err
	}
	if len(points) > 0 {
		return points, res, nil
	}
	derived, crossRes, err := s.fiatSeriesThroughXLM(ctx, pair, win, read)
	if err != nil {
		return nil, chartWalkResult{}, err
	}
	if len(derived) > 0 {
		return derived, crossRes, nil
	}
	res.degraded = res.degraded || crossRes.degraded
	return nil, res, nil
}

// chartObservedPoints merges every DIRECTLY OBSERVED source (alias spellings, then fiat-quote stablecoin proxies) with no
// derivation, so a [Server.fiatSeriesThroughXLM] leg can read it without recursing.
func (s *Server) chartObservedPoints(
	ctx context.Context,
	pair canonical.Pair,
	win chartWindow,
	read chartRead,
) ([]HistoryPoint, chartWalkResult, error) {
	w := s.newChartWalk(pair, win, read)
	if err := s.chartMergeAliasPairs(ctx, w); err != nil {
		return nil, chartWalkResult{}, err
	}
	if err := s.chartStablecoinFallback(ctx, w); err != nil {
		return nil, chartWalkResult{}, err
	}
	return w.merge.series(), w.res, nil
}

// chartStablecoinFallback handles X/fiat via X/<proxy> (the stablecoin-to-fiat mapping is aggregator policy applied at read
// time). It runs on EVERY fiat-quoted request, since a series with a hole is not empty; [chartBucketMerge] stops a proxy
// displacing a bucket the requested quote answered. It must never COST the series: reads once one is held are bounded by
// [chartWalkBudget], failures mark the response degraded, and the walk stops at [chartWindow.covered].
func (s *Server) chartStablecoinFallback(ctx context.Context, w *chartWalk) error {
	if w.pair.Quote.Type != canonical.AssetFiat {
		return nil
	}
	for _, pp := range s.chartFiatProxyPairs(w.pair) {
		cont, err := w.step(ctx, pp, chartSourceProxy)
		if err != nil {
			return err
		}
		if !cont {
			return nil
		}
	}
	return nil
}

// chartFiatProxyPairs is the ordered proxy list for a fiat-quoted series, including abstract stablecoin backers. Order is deterministic
// across regions (ADR-0015) and load-bearing, as the first pair holding a bucket owns it: pass 1 ESTABLISHED spellings (classic pegs in
// config order, then sorted aggregate.FiatBackers), pass 2 HELD-BACK spellings (a peg's SAC wrapper) across ALL families, so one family's
// thin SAC pool cannot own a bucket another's classic book answers. The base leg keeps [canonical.AssetAliases] order (SAC last).
func (s *Server) chartFiatProxyPairs(pair canonical.Pair) []canonical.Pair {
	established, heldBack := s.chartFiatProxyQuotes(pair.Quote)
	out := s.chartProxyPairsFor(pair, established, nil)
	return s.chartProxyPairsFor(pair, heldBack, out)
}

// chartFiatProxyQuotes splits a fiat quote's proxy assets into established and held-back
// spellings; see [Server.chartFiatProxyPairs].
func (s *Server) chartFiatProxyQuotes(quote canonical.Asset) (established, heldBack []canonical.Asset) {
	seen := make(map[string]struct{})
	add := func(dst *[]canonical.Asset, a canonical.Asset) {
		k := a.String()
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		*dst = append(*dst, a)
	}
	// (1a) operator classic pegs in their priority-first form — USD only
	// (they carry issuer identity and are mapped to fiat only for USD by
	// the operator's allow-list).
	if quote.Code == "USD" {
		for _, peg := range s.USDPeggedClassics {
			add(&established, canonical.CanonicalAsset(peg))
		}
	}
	// (1b) abstract stablecoin backers for the quote's fiat, in
	// FiatBackers' sorted order.
	for _, code := range aggregate.FiatBackers(quote.Code) {
		if a, err := canonical.NewCryptoAsset(code); err == nil {
			add(&established, a)
		}
	}
	// (2) the remaining canonical forms of each declared peg — the SAC
	// wrappers — after every established spelling of every family.
	if quote.Code == "USD" {
		for _, peg := range s.USDPeggedClassics {
			for _, form := range assetAliases(peg) {
				add(&heldBack, form)
			}
		}
	}
	return established, heldBack
}

// chartProxyPairsFor crosses one pass of proxy quotes with every base alias, appending to `out`
// with a shared dedupe so a market reached by both passes stays with the pass read first.
func (s *Server) chartProxyPairsFor(
	pair canonical.Pair, quotes []canonical.Asset, out []canonical.Pair,
) []canonical.Pair {
	literal := pair.Base.String() + "\x00" + pair.Quote.String()
	seen := make(map[string]struct{}, len(out))
	for _, p := range out {
		seen[p.Base.String()+"\x00"+p.Quote.String()] = struct{}{}
	}
	for _, b := range assetAliases(pair.Base) {
		for _, q := range quotes {
			if sameAsset(q, b) {
				continue
			}
			pp, err := canonical.NewPair(b, q)
			if err != nil {
				continue
			}
			k := pp.Base.String() + "\x00" + pp.Quote.String()
			if k == literal {
				continue // the alias walk already read the literal pair
			}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, pp)
		}
	}
	return out
}

// chartAliasPairs is the requested pair in every canonical spelling of both legs (XLM dual-form), in [canonical.AssetAliases]
// order (literal first, SAC last), identity dropped. Separate from the read so [Server.chartCoverageSet] enumerates exactly
// what serving reads.
func (s *Server) chartAliasPairs(pair canonical.Pair) []canonical.Pair {
	out := make([]canonical.Pair, 0, len(assetAliases(pair.Base))*len(assetAliases(pair.Quote)))
	for _, b := range assetAliases(pair.Base) {
		for _, q := range assetAliases(pair.Quote) {
			ap, err := canonical.NewPair(b, q)
			if err != nil {
				continue // degenerate alias combination (identity pair)
			}
			out = append(out, ap)
		}
	}
	return distinctMarkets(out)
}

// chartMergeAliasPairs reads every alias spelling of the pair into `m`, each bucket going to the highest-priority spelling
// holding it (a literal read misses `native` vs `crypto:XLM/<quote>` venues). An alias is not a proxy, so flags.triangulated
// stays false; spellings rank per BUCKET, never blend. An alias error (e.g. [ErrUnknownGranularity]) propagates; once one has
// answered, a later failure degrades ([chartSourceClass]).
func (s *Server) chartMergeAliasPairs(ctx context.Context, w *chartWalk) error {
	for _, ap := range s.chartAliasPairs(w.pair) {
		cont, err := w.step(ctx, ap, chartSourceAlias)
		if err != nil {
			return err
		}
		if !cont {
			return nil
		}
	}
	return nil
}

// chartVWAPReader returns a [chartStablecoinFallback] read closure that
// fetches a pair's closed prices_<gran> series over [from, to).
func (s *Server) chartVWAPReader(gran string) chartRead {
	return func(ctx context.Context, p canonical.Pair, from, to time.Time, limit int) ([]HistoryPoint, error) {
		return s.History.HistoryPointsInRange(ctx, p, gran, from, to, limit)
	}
}

// fiatSeriesThroughXLM derives a fiat-quoted series by crossing asset/XLM with XLM/CCY per bucket (XLM-pivot twin of
// tryUSDAnchoredFiatCross, ADR-0051), for a declared USD peg with no USD leg; the fiat fallback's LAST route, sets flags.triangulated. The
// traded price shows a depeg a flat 1.0 would hide. Both legs use [Server.chartObservedPoints] (no cross of crosses), asset leg first; only
// buckets on BOTH legs emit; a leg's alias-class error is returned, not served as empty.
func (s *Server) fiatSeriesThroughXLM(
	ctx context.Context, pair canonical.Pair, win chartWindow,
	read chartRead,
) ([]HistoryPoint, chartWalkResult, error) {
	legs, ok := fiatCrossLegsThroughXLM(pair)
	if !ok {
		return nil, chartWalkResult{}, nil
	}
	assetPts, assetRes, err := s.chartObservedPoints(ctx, legs[0], win, read)
	if err != nil {
		return nil, chartWalkResult{}, err
	}
	// A leg that stopped short makes the PRODUCT short (or empty): the
	// cross emits only buckets present on both, so carry each leg's
	// degradation out even when the cross comes back empty.
	res := chartWalkResult{degraded: assetRes.degraded, cappedAt: assetRes.cappedAt}
	if len(assetPts) == 0 {
		return nil, res, nil
	}
	xlmPts, xlmRes, err := s.chartObservedPoints(ctx, legs[1], win, read)
	if err != nil {
		return nil, chartWalkResult{}, err
	}
	res.degraded = res.degraded || xlmRes.degraded
	res.mergeCap(assetRes.cappedAt)
	res.mergeCap(xlmRes.cappedAt)
	if len(xlmPts) == 0 {
		return nil, res, nil
	}
	crossed := crossSeriesThroughPivot(assetPts, xlmPts)
	res.proxied = len(crossed) > 0
	return crossed, res, nil
}

// fiatCrossLegsThroughXLM is the gate and leg list of [Server.fiatSeriesThroughXLM] (fiat-quoted, non-fiat, non-XLM base gives
// base/XLM and XLM/quote), shared so [Server.chartCoverageSet] enumerates the SAME legs.
func fiatCrossLegsThroughXLM(pair canonical.Pair) ([2]canonical.Pair, bool) {
	if pair.Quote.Type != canonical.AssetFiat || pair.Base.Type == canonical.AssetFiat {
		return [2]canonical.Pair{}, false
	}
	xlm := canonical.NativeAsset()
	if sameAsset(pair.Base, xlm) {
		return [2]canonical.Pair{}, false
	}
	assetLeg, err := canonical.NewPair(pair.Base, xlm)
	if err != nil {
		return [2]canonical.Pair{}, false
	}
	xlmLeg, err := canonical.NewPair(xlm, pair.Quote)
	if err != nil {
		return [2]canonical.Pair{}, false
	}
	return [2]canonical.Pair{assetLeg, xlmLeg}, true
}

// crossSeriesThroughPivot merge-joins two ascending series into base/pivot x pivot/quote per shared bucket; a bucket missing on
// either side is skipped, never carried forward. VolumeUSD is the base leg's.
func crossSeriesThroughPivot(basePts, pivotPts []HistoryPoint) []HistoryPoint {
	n := len(basePts)
	if len(pivotPts) < n {
		n = len(pivotPts)
	}
	out := make([]HistoryPoint, 0, n)
	i, j := 0, 0
	for i < len(basePts) && j < len(pivotPts) {
		b, p := basePts[i], pivotPts[j]
		switch {
		case b.Bucket.Before(p.Bucket):
			i++
		case p.Bucket.Before(b.Bucket):
			j++
		default:
			i++
			j++
			crossed, ok := crossThroughPivot(b.VWAP, p.VWAP)
			if !ok {
				continue
			}
			out = append(out, HistoryPoint{
				Bucket:    b.Bucket,
				VWAP:      crossed,
				VolumeUSD: b.VolumeUSD,
				Sources:   unionSources(b.Sources, p.Sources),
			})
		}
	}
	return out
}

// crossThroughPivot is the one pivot multiplication on NUMERIC text: exact big.Rat (ADR-0003) via [ratToDecimal]. ok=false when
// a leg is unparseable or not strictly positive (a miss, not a zero). Shared by series and point surfaces so rounding and
// refusals cannot drift.
func crossThroughPivot(basePerPivot, pivotPerQuote string) (string, bool) {
	br, pr := ratFromDecimal(basePerPivot), ratFromDecimal(pivotPerQuote)
	if br == nil || pr == nil || br.Sign() <= 0 || pr.Sign() <= 0 {
		return "", false
	}
	return ratToDecimal(new(big.Rat).Mul(br, pr), ohlcPriceDigits), true
}

// adjustHistoryPointPrices applies the nonstandard-decimals normalisation to each VWAP (docs/operations/runbooks/dex.md);
// VolumeUSD is untouched (already USD). Equal decimals return points UNCHANGED for byte-identical output, as the CAGG's
// NUMERIC::text differs from [ratToDecimal]'s fixed 10 digits.
func adjustHistoryPointPrices(points []HistoryPoint, baseDecimals, quoteDecimals int) []HistoryPoint {
	if baseDecimals == quoteDecimals || len(points) == 0 {
		return points
	}
	out := make([]HistoryPoint, len(points))
	for i, p := range points {
		out[i] = p
		out[i].VWAP = adjustOHLCPriceString(p.VWAP, baseDecimals, quoteDecimals)
	}
	return out
}

// parseChartPair builds the canonical Pair from query params,
// rejecting identity pairs. ok=false on any error (problem written).
func parseChartPair(w http.ResponseWriter, r *http.Request) (canonical.Pair, bool) {
	asset, quote, ok := parseChartAssetQuote(w, r)
	if !ok {
		return canonical.Pair{}, false
	}
	if asset.Equal(quote) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/identity-pair",
			"Asset is the quote", http.StatusBadRequest,
			"asset and quote must differ")
		return canonical.Pair{}, false
	}
	pair, err := canonical.NewPair(asset, quote)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-pair",
			"Invalid pair", http.StatusBadRequest, err.Error())
		return canonical.Pair{}, false
	}
	return pair, true
}

// parseChartParams resolves timeframe, granularity and price_type with ADR-0020 defaults; ok=false after writing the problem.
func parseChartParams(w http.ResponseWriter, r *http.Request) (string, chartTimeframeSpec, string, string, bool) {
	tfRaw := r.URL.Query().Get("timeframe")
	if tfRaw == "" {
		tfRaw = "24h"
	}
	tf, ok := chartTimeframes[tfRaw]
	if !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-timeframe",
			"Invalid timeframe", http.StatusBadRequest,
			fmt.Sprintf("timeframe must be one of: 1h, 24h, 1w, 1mo, 1y, all (got %q)", tfRaw))
		return "", chartTimeframeSpec{}, "", "", false
	}
	gran := r.URL.Query().Get("granularity")
	if gran == "" {
		gran = tf.DefaultGranule
	}
	priceType := r.URL.Query().Get("price_type")
	if priceType == "" {
		priceType = "vwap"
	}
	switch priceType {
	case "vwap":
		// Default price series — the fall-through path in handleChart.
	case "twap":
		// Time-weighted series — dispatched to handleChartTWAP, backed by
		// the twap_1h / twap_1d CAGGs (migration 0081). parseChartParams
		// just accepts the token here.
	case "market_cap":
		// Separate compute path — the handler dispatches to
		// handleChartMarketCap before falling through to the vwap-path.
	default:
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-price-type",
			"Invalid price_type", http.StatusBadRequest,
			fmt.Sprintf("price_type must be one of: vwap, twap, market_cap (got %q)", priceType))
		return "", chartTimeframeSpec{}, "", "", false
	}
	return tfRaw, tf, gran, priceType, true
}

// parseChartAssetQuote parses `asset` (required) and `quote` (default defaultPriceQuote); ok=false after writing the problem.
func parseChartAssetQuote(w http.ResponseWriter, r *http.Request) (canonical.Asset, canonical.Asset, bool) {
	rawAsset, ok := resolveAssetOrBaseParam(w, r)
	if !ok {
		return canonical.Asset{}, canonical.Asset{}, false
	}
	asset, err := canonical.ParseAsset(rawAsset)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset identifier", http.StatusBadRequest, err.Error())
		return canonical.Asset{}, canonical.Asset{}, false
	}
	quote := defaultPriceQuote
	if rawQuote := r.URL.Query().Get("quote"); rawQuote != "" {
		q, err := canonical.ParseAsset(rawQuote)
		if err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-quote",
				"Invalid quote identifier", http.StatusBadRequest, err.Error())
			return canonical.Asset{}, canonical.Asset{}, false
		}
		quote = q
	}
	return asset, quote, true
}

// handleChartMarketCap serves price_type=market_cap. Fiat base: daily M2 (catalogue) x inverse_usd (fx_quotes); non-fiat goes
// to handleChartMarketCapCrypto. The quote is always fiat:USD.
func (s *Server) handleChartMarketCap(
	w http.ResponseWriter,
	r *http.Request,
	pair canonical.Pair,
	tfRaw, gran string,
	from time.Time,
) {
	// Quote must be fiat:USD — market cap is USD-denominated.
	if pair.Quote.Type != canonical.AssetFiat || pair.Quote.Code != "USD" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-market-cap-quote",
			"market_cap requires quote=fiat:USD", http.StatusBadRequest,
			"the chart's price_type=market_cap series is always USD-denominated; pass quote=fiat:USD")
		return
	}

	// Non-fiat (on-chain) base → crypto market-cap-over-time: daily
	// USD price (the existing prices_1d / stablecoin-proxy series) ×
	// daily circulating supply (supply_1d CAGG, migration 0066).
	if pair.Base.Type != canonical.AssetFiat {
		s.handleChartMarketCapCrypto(w, r, pair, tfRaw, from)
		return
	}

	if s.VerifiedCurrencies == nil || s.FXHistory == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/market-cap-unavailable",
			"market_cap not configured", http.StatusServiceUnavailable,
			"this deployment hasn't wired the verified-currency catalogue and/or fx_quotes reader")
		return
	}

	vc, ok := s.VerifiedCurrencies.LookupByTicker(pair.Base.Code)
	if !ok || vc.CirculatingSupply == "" {
		writeChartJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{})
		return
	}
	// ADR-0003: the catalogue carries supply as an exact decimal STRING; float64 would truncate
	// past 53 bits (a quadrillion-unit fiat M2). Scale by supply_decimals via big.Rat.
	m2, ok := fiatSupplyWholeUnits(vc.CirculatingSupply, vc.SupplyDecimals)
	if !ok {
		s.logger.Warn("market_cap: bad catalogue supply",
			"ticker", vc.Ticker, "supply", vc.CirculatingSupply)
		writeChartJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{})
		return
	}

	// Default window: trailing 25y (ECB inception), the same as the direct
	// fiat path, when timeframe=all.
	to := time.Now().UTC().Truncate(24 * time.Hour)
	queryFrom := from
	if queryFrom.IsZero() {
		queryFrom = to.AddDate(-25, 0, 0)
	}

	fxCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	points, err := s.FXHistory.ListFXHistory(fxCtx, pair.Base.Code, queryFrom, to)
	if err != nil {
		s.marketCapReadFailed(w, r, fxCtx, err, "ListFXHistory", pair, tfRaw, gran, from, "market_cap: fx_quotes fetch failed", "ticker", pair.Base.Code, "err", err)
		return
	}

	wire := make([]HistoryPointWire, 0, len(points))
	for _, p := range points {
		// market_cap = supply × rate, exact big.Rat over the stored
		// NUMERIC rate text — no float anywhere in the product.
		rate, ok := p.inverseUSDRat()
		if !ok {
			continue
		}
		wire = append(wire, HistoryPointWire{
			T: WireTime(p.Bucket),
			P: new(big.Rat).Mul(m2, rate).FloatString(2),
		})
	}

	series := ChartSeries{
		AssetID:     pair.Base.String(),
		Quote:       pair.Quote.String(),
		Timeframe:   tfRaw,
		Granularity: gran,
		PriceType:   "market_cap",
		Points:      wire,
	}
	if !from.IsZero() && len(wire) > 0 {
		if grace := chartGranularityGrace(gran); wire[0].T.Time().Sub(from) > grace {
			startsAt := wire[0].T
			requested := from
			series.Truncated = true
			series.DataStartsAt = &startsAt
			series.RequestedFrom = wireTimePtr(&requested)
		}
	}
	writeChartJSON(w, series, Flags{})
}

// writeChartTimeout answers a chart read that blew its budget with a retryable 503 rather than an empty 200 with no flag: a
// deadline is capacity, not absence of data.
func (s *Server) writeChartTimeout(w http.ResponseWriter, r *http.Request, leg, ticker string) {
	w.Header().Set("Retry-After", "5")
	writeProblem(w, r, "https://api.stellarindex.io/errors/chart-timeout", "Chart query timed out",
		http.StatusServiceUnavailable,
		fmt.Sprintf("%s for %s did not return inside the request budget; retry shortly.", leg, ticker))
}

// marketCapReadFailed triages a failed leg read: client hangup writes nothing; a blown budget gets the `chart-timeout` 503;
// anything else is an empty series flagged stale, not presented as fact.
func (s *Server) marketCapReadFailed(w http.ResponseWriter, r *http.Request, ctx context.Context, err error, leg string, pair canonical.Pair, tfRaw, gran string, from time.Time, msg string, kv ...any) {
	if clientAborted(r, err) {
		return
	}
	if handlerTimedOut(ctx, err) {
		s.writeMarketCapTimeout(w, r, leg, pair, tfRaw, gran)
		return
	}
	s.logger.Warn(msg, append(kv, "err", err)...)
	writeChartJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{Stale: true, Degraded: true})
}

// writeMarketCapTimeout answers a blown market-cap deadline with the same `chart-timeout` 503 as vwap/twap: an empty 200 would
// render "market cap $0" for an asset with real supply. Only a 5xx says retry.
func (s *Server) writeMarketCapTimeout(w http.ResponseWriter, r *http.Request, leg string, pair canonical.Pair, tfRaw, gran string) {
	s.logger.Warn("market_cap crypto: deadline exceeded",
		"leg", leg, "asset", pair.Base.String(), "quote", pair.Quote.String(),
		"timeframe", tfRaw, "granularity", gran)
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/chart-timeout",
		"Chart query timed out", http.StatusServiceUnavailable,
		"the market-cap series' price + supply reads didn't both return inside the request budget; retry shortly.")
}

// emptyMarketCapSeries is the no-data shape (empty points array, not null). Callers reaching it from a read FAILURE must set
// flags.stale: unflagged it claims "no market cap", not "could not compute".
func emptyMarketCapSeries(pair canonical.Pair, tfRaw, gran string, _ time.Time) ChartSeries {
	return ChartSeries{
		AssetID:     pair.Base.String(),
		Quote:       pair.Quote.String(),
		Timeframe:   tfRaw,
		Granularity: gran,
		PriceType:   "market_cap",
		Points:      []HistoryPointWire{},
	}
}

// handleChartMarketCapCrypto serves market_cap for an on-chain base: daily USD price (prices_1d, stablecoin-USD proxy fallback)
// x forward-filled supply_1d. Off-chain crypto:* assets have no on-chain supply (supply.AssetKey errors) and return an empty
// series, not a fabricated cap.
func (s *Server) handleChartMarketCapCrypto(
	w http.ResponseWriter,
	r *http.Request,
	pair canonical.Pair,
	tfRaw string,
	from time.Time,
) {
	const gran = "1d" // market cap is always a daily series

	if s.History == nil || s.Supply == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/market-cap-unavailable",
			"market_cap not configured", http.StatusServiceUnavailable,
			"this deployment hasn't wired the history + supply readers needed for crypto market-cap")
		return
	}

	supplyKey, err := supply.AssetKey(pair.Base)
	if err != nil {
		// Off-chain reference asset — no on-chain supply to multiply.
		writeChartJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// USD price series (daily), with the stablecoin-USD proxy fallback
	// the normal chart uses when nothing trades directly in fiat:USD.
	pricePts, walk, err := s.chartSeriesPoints(ctx, pair,
		chartWindow{from: from, gran: gran}, s.chartVWAPReader(gran))
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(ctx, err) {
			s.writeMarketCapTimeout(w, r, "HistoryPointsInRange", pair, tfRaw, gran)
			return
		}
		s.logger.Warn("market_cap crypto: price history failed",
			"asset", pair.Base.String(), "err", err)
		writeChartJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{Stale: true, Degraded: true})
		return
	}
	// The price leg is already normalised per SOURCE pair ([Server.adjustSourcePoints]). baseDec is
	// for the SUPPLY leg: supply is in the base token's smallest unit, so divide by 10^baseDec, not
	// a hardcoded 10^7, or the cap is off by 10^(baseDec-7).
	//
	// LATENT: [NonstandardDecimalsCache.Lookup] does NOT fold aliases, so a table naming one
	// spelling of an aliasing family but not another would put price and supply a power of ten
	// apart. Unreachable while every row is a bare C... contract id.
	baseDec := aggregate.ResolveDecimals(s.NonstandardDecimals, pair.Base)

	// Daily circulating supply (forward-filled via the carry-in row).
	to := time.Now().UTC().Truncate(24 * time.Hour)
	supPts, err := s.Supply.DailyCirculatingSupply(ctx, supplyKey, from, to)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(ctx, err) {
			s.writeMarketCapTimeout(w, r, "DailyCirculatingSupply", pair, tfRaw, gran)
			return
		}
		s.logger.Warn("market_cap crypto: supply history failed",
			"asset_key", supplyKey, "err", err)
		writeChartJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{Stale: true, Degraded: true})
		return
	}

	wire, supplyStale := marketCapPoints(pricePts, supPts, baseDec)
	refused, lowLiquidity := s.marketCapSeriesRefused(ctx, pair.Base, wire)
	if refused {
		wire = []HistoryPointWire{}
	}
	series := ChartSeries{
		AssetID:               pair.Base.String(),
		Quote:                 pair.Quote.String(),
		Timeframe:             tfRaw,
		Granularity:           gran,
		PriceType:             "market_cap",
		Points:                wire,
		MarketCapLowLiquidity: lowLiquidity,
	}
	if !from.IsZero() && len(wire) > 0 {
		if grace := chartGranularityGrace(gran); wire[0].T.Time().Sub(from) > grace {
			startsAt := wire[0].T
			requested := from
			series.Truncated = true
			series.DataStartsAt = &startsAt
			series.RequestedFrom = wireTimePtr(&requested)
		}
	}
	if !refused {
		series.markRowCap(walk)
	}
	writeChartJSON(w, series, Flags{Triangulated: walk.proxied, Stale: walk.degraded || supplyStale, Degraded: walk.degraded})
}

// marketCapSeriesRefused applies the detail page's valuation guards ([Server.marketCapRefused], then the turnover ceiling on
// the latest cap) so the chart cannot plot a valuation the headline refuses. Today's verdict covers the whole series (inputs
// are current readings); unmeasured inputs never refuse.
func (s *Server) marketCapSeriesRefused(ctx context.Context, base canonical.Asset, wire []HistoryPointWire) (refused, lowLiquidity bool) {
	if len(wire) == 0 {
		return false, false
	}
	var probe AssetDetail
	if s.MinMarketCapVolumeUSD > 0 || s.MaxMarketCapVolumeRatio > 0 {
		s.populateVolume24h(ctx, &probe, base)
	}
	sources := 0
	if probe.VolumeUSD24h != nil && s.MinMarketCapVolumeUSD > 0 {
		sources = s.lookupUSDPriceWithSources(ctx, base).sources
	}
	if refused, lowLiquidity = s.marketCapRefused(base, sources, probe.VolumeUSD24h); refused {
		return refused, lowLiquidity
	}
	if capExceedsObservedTurnover(wire[len(wire)-1].P, probe.VolumeUSD24h, s.MaxMarketCapVolumeRatio) {
		return true, true
	}
	return false, false
}

// marketCapSupplyMaxCarry bounds forward-filling a supply_1d bucket onto later price days: two days covers the post-midnight
// CAGG refresh window; beyond it the observer has missed days and the product would pair today's price with an unmeasured
// supply.
const marketCapSupplyMaxCarry = 48 * time.Hour

// marketCapPoints multiplies the daily USD price by the latest supply at-or-before each day, carried at most
// marketCapSupplyMaxCarry; inputs ascending. A day with no usable supply is skipped, never zero; supplyStale reports that the
// carry bound cut a day.
func marketCapPoints(pricePts []HistoryPoint, supPts []timescale.SupplyDayPoint, baseDecimals int) (wire []HistoryPointWire, supplyStale bool) {
	wire = make([]HistoryPointWire, 0, len(pricePts))
	si := 0
	var cur *big.Int
	var curAt time.Time
	for _, pp := range pricePts {
		for si < len(supPts) && !supPts[si].Bucket.After(pp.Bucket) {
			cur, curAt = supPts[si].Circulating, supPts[si].Bucket
			si++
		}
		if cur == nil || pp.VWAP == "" {
			continue
		}
		if pp.Bucket.Sub(curAt) > marketCapSupplyMaxCarry {
			supplyStale = true
			continue
		}
		mc, err := usdMarketValue(cur, pp.VWAP, baseDecimals)
		if err != nil {
			continue
		}
		wire = append(wire, HistoryPointWire{T: WireTime(pp.Bucket), P: mc})
	}
	return wire, supplyStale
}

// fiatSupplyWholeUnits converts (supply, decimals) into an EXACT whole-unit big.Rat (ADR-0003: float64 truncates past 2^53);
// ok=false when supplyStr is not a decimal or decimals is negative.
func fiatSupplyWholeUnits(supplyStr string, decimals int) (*big.Rat, bool) {
	if decimals < 0 {
		return nil, false
	}
	v, ok := new(big.Rat).SetString(supplyStr)
	if !ok {
		return nil, false
	}
	if decimals > 0 {
		div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
		v.Quo(v, new(big.Rat).SetInt(div))
	}
	return v, true
}

// fxSeriesCredit is creditForSources over the feeds behind a set of FX points.
func fxSeriesCredit(points []FXQuotePoint) string {
	sources := make([]string, 0, len(points))
	for _, p := range points {
		sources = append(sources, p.Source)
	}
	return creditForSources(sources)
}
