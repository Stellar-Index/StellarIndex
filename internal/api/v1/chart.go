package v1

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// ChartSeries is the wire shape for /v1/chart. Mirrors the OpenAPI
// ChartEnvelope.data shape. See ADR-0020 for the contract decision.
//
// `truncated` + `data_starts_at` signal that the requested timeframe
// extends beyond the deployment's actual retention. R1 today only
// has ~7 days of high-resolution history but still accepts
// `?timeframe=1y` — without these fields a consumer can't tell
// whether the returned 7 daily points are "the last 7 days of a
// long history" or "all the history this deployment has". R-013 in
// `docs/review-2026-05-10.md`.
type ChartSeries struct {
	AssetID       string             `json:"asset_id"`
	Quote         string             `json:"quote"`
	Timeframe     string             `json:"timeframe"`
	Granularity   string             `json:"granularity"`
	PriceType     string             `json:"price_type"` // "vwap" | "twap" | "market_cap"
	Points        []HistoryPointWire `json:"points"`
	Truncated     bool               `json:"truncated"`                // true when the requested window starts before the earliest available data
	DataStartsAt  *time.Time         `json:"data_starts_at,omitempty"` // earliest bucket timestamp present in the result; only populated when Truncated
	RequestedFrom *time.Time         `json:"requested_from,omitempty"` // window start the consumer asked for; only populated when Truncated
	Discontinuous bool               `json:"discontinuous"`            // true when points skip at least one whole bucket between two served buckets
	GapStartsAt   *time.Time         `json:"gap_starts_at,omitempty"`  // last bucket before the WIDEST interior gap; only populated when Discontinuous
	GapEndsAt     *time.Time         `json:"gap_ends_at,omitempty"`    // first bucket after it; only populated when Discontinuous
}

// markDiscontinuity stamps the interior-gap signal onto a series that is
// about to be written.
//
// `points` is an array with no holes in it, so a consumer plots it as a
// continuous line whether or not the buckets between two entries exist.
// That is the right rendering for a market that was quiet and the wrong
// one for a series the deployment cannot answer over part of its own
// span, and before this signal the two were indistinguishable on the
// wire: `truncated` describes only the series' START (and is
// deliberately never raised for `timeframe=all`), and the coverage
// annotation ([Server.coverageAnnotationIfEmpty]) speaks only for a
// series that is EMPTY. A 1,070-point `native/fiat:USD` daily chart
// carrying one 1,919-day break serialised as neither.
//
// The threshold is the NEXT BUCKET on the served granularity's own grid
// ([chartBucketStep]), not a fixed duration: `prices_1mo` is built with
// `time_bucket('1 month', ts, 'UTC')`, so its buckets are CALENDAR
// months and every 31-day one is longer than any fixed month-sized
// grace. Measured against a fixed 30-day grace, 7 of the 12 adjacencies
// in a contiguous 2025 monthly series tripped the flag — a series with
// no hole in it reporting `discontinuous: true` and naming
// 2025-01-01 → 2025-02-01 as its widest gap, which is the field's own
// documented meaning inverted. Every other grain is fixed-width in UTC
// and unaffected either way.
//
// Only the WIDEST gap is reported, mirroring `truncated`'s single
// (data_starts_at, requested_from) pair rather than putting an unbounded
// list on the wire; a consumer that needs every gap has the buckets
// themselves.
//
// The signal is a statement about THIS response at THIS granularity: a
// quiet market at `1m` is genuinely discontinuous, and a caller that
// wants to distinguish "quiet" from "unheld" reads it beside
// `coverage_from`. A granularity with no known grid emits nothing
// rather than guessing — a false gap claim is worse than none.
func (c *ChartSeries) markDiscontinuity() {
	for i := 1; i < len(c.Points); i++ {
		next := chartBucketStep(c.Points[i-1].T, c.Granularity)
		if next.IsZero() {
			return // unknown grain: no grid to measure a gap against
		}
		if !c.Points[i].T.After(next) {
			continue // the very next bucket (or the same one)
		}
		gap := c.Points[i].T.Sub(c.Points[i-1].T)
		if c.Discontinuous && gap <= c.GapEndsAt.Sub(*c.GapStartsAt) {
			continue
		}
		from, to := c.Points[i-1].T, c.Points[i].T
		c.Discontinuous = true
		c.GapStartsAt = &from
		c.GapEndsAt = &to
	}
}

// chartBucketStep returns the START of the bucket that immediately
// follows the one starting at t, on the granularity's own grid — the
// same grid `time_bucket(<gran>, ts, 'UTC')` lays down in the CAGGs.
// The zero time means the grain has no known grid.
//
// `1mo` advances by a CALENDAR month and `1w` / `1d` by whole UTC days,
// which is what Timescale's own bucketing does; the sub-day grains are
// fixed multiples and are exact either way. This is deliberately NOT
// [chartGranularityGrace]: that function answers a different question
// (how far after `from` the first bucket may start before the series
// counts as retention-truncated) with a single fixed duration per
// grain, and a fixed 30-day "month" is wrong for 7 months of every
// year.
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

// writeChartJSON is the single write seam for every /v1/chart response
// that carries a series, so no chart shape can reach the wire without
// its interior-gap signal ([ChartSeries.markDiscontinuity]) computed.
// The default vwap path goes through [Server.writeChartSeries] instead,
// which adds the coverage annotation and stamps the same signal —
// gating on the handler to remember would be how a variant added later
// silently ships an unannotated hole, which is the class this whole
// change closes.
func writeChartJSON(w http.ResponseWriter, series ChartSeries, flags Flags) {
	series.markDiscontinuity()
	writeJSON(w, series, flags)
}

// chartTimeframeSpec captures what each prescribed timeframe
// translates to: a window duration and a default granularity.
// `all` has zero duration → no lower bound (since-inception).
type chartTimeframeSpec struct {
	Duration       time.Duration
	DefaultGranule string
}

// chartTimeframes is the canonical timeframe → spec table per
// ADR-0020. Adding a new timeframe is a one-line change here plus
// an OpenAPI enum update.
var chartTimeframes = map[string]chartTimeframeSpec{
	"1h":  {Duration: time.Hour, DefaultGranule: "1m"},
	"24h": {Duration: 24 * time.Hour, DefaultGranule: "15m"},
	"1w":  {Duration: 7 * 24 * time.Hour, DefaultGranule: "1h"},
	"1mo": {Duration: 30 * 24 * time.Hour, DefaultGranule: "4h"},
	"1y":  {Duration: 365 * 24 * time.Hour, DefaultGranule: "1d"},
	"all": {Duration: 0, DefaultGranule: "1d"},
}

// chartWithheldForScam applies the directory-scam gate to /v1/chart,
// writing the withheld problem and reporting true when the series must
// not be served.
//
// /v1/chart served a full price SERIES for a flagged issuer while
// /v1/price, /v1/price/tip, /v1/price/batch, /v1/vwap, /v1/twap, the
// SEP-40 oracle and the asset headline all withheld it (#366). A series
// is arguably worse than a point: withholding one number denies a quote,
// but an ungated chart hands over the whole trajectory, which is what
// makes a manufactured market look legitimate.
//
// Called from handleChart after the pair is known and BEFORE
// dispatchSpecialisedChart, which is the load-bearing placement. Keying
// on the BASE (not the pair) survives the frontend's XLM triangulation,
// so a flagged asset cannot slip through against a different quote. And
// sitting ahead of the dispatch covers the default path plus every
// specialised variant (market-cap, fiat-cross, TWAP) with ONE check —
// gating each variant separately is precisely how this class keeps
// recurring, since a surface added later simply misses it. This is its
// third appearance: MSP-02 found /v1/vwap and /v1/twap ungated after
// pricingguard/scam.go's own doc claimed a single reader-seam gate
// covered everything, which was never true of endpoints that compute
// from their own fetch. /v1/chart reads history directly, same shape.
//
// Deliberately NOT pushed down into the history reader: that reader also
// backs /v1/history and /v1/observations, and scam.go, substance.go and
// the withheld problem's own guidance text all promise those raw
// surfaces stay visible. Gating there would make our own error
// message's escape-hatch advice a lie — the same reasoning vwap.go
// records for tradesInRangeWithStablecoinFallback.
//
// Extracted rather than inlined because inlining pushed handleChart to
// cognitive complexity 21 against the package's ceiling of 20. The lint
// was right: the handler already dispatches four ways.
func (s *Server) chartWithheldForScam(w http.ResponseWriter, r *http.Request, pair canonical.Pair) bool {
	if s.scam == nil || !s.scam.Withheld(r.Context(), pair.Base, "chart") {
		return false
	}
	writePriceWithheldProblem(w, r, pair.Base, pair.Quote)
	return true
}

// handleChart serves
// GET /v1/chart?asset=<id>&quote=<id>&timeframe=<tf>&granularity=<g>&price_type=<pt>
//
// Defaults: quote=USD, timeframe=24h, granularity=(per timeframe
// table), price_type=vwap. Response is a CAGG-served series of
// CLOSED buckets (ADR-0015) within the timeframe window.
//
// price_type=twap is served from the twap_1h / twap_1d CAGGs
// (migration 0081) via handleChartTWAP — snapped to a 1h or 1d grain;
// price_type=market_cap routes to handleChartMarketCap. Both are
// dispatched in dispatchSpecialisedChart before the default vwap path.
func (s *Server) handleChart(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
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

	if s.chartWithheldForScam(w, r, pair) {
		return
	}

	tfRaw, tf, gran, priceType, ok := parseChartParams(w, r)
	if !ok {
		return
	}

	var from time.Time
	if tf.Duration > 0 {
		from = time.Now().Add(-tf.Duration).UTC()
	}

	// Dispatch to specialised handlers when the request shape calls
	// for it; fall through to the default vwap-on-prices_1m path
	// when no specialisation matches.
	if s.dispatchSpecialisedChart(w, r, pair, tfRaw, gran, priceType, from) {
		return
	}

	// Serve the finest grain the requested window can actually carry
	// ([chartFitGranularity]). `gran` is REPLACED rather than shadowed:
	// every use below it — the read, the window the walk covers, the
	// retention grace, the `granularity` on the wire and the two log
	// lines — is about the series that is actually produced, and the
	// 400 body is unaffected because an unknown grain is returned
	// untouched.
	gran = chartFitGranularity(chartVWAPGranularityLadder, gran, tf.Duration)

	// 8s ceiling on the chart query + downstream stablecoin
	// fallback. Same pattern as #1082 / #1099 / #1100 / #1101.
	// The chart's prices_1m / prices_5m / prices_1h scan can take
	// 5–10s on a cold cache for long timeframes (`?timeframe=1y`
	// + `granularity=1h` is ~8 760 buckets).
	chartCtx, chartCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer chartCancel()
	points, walk, err := s.chartSeriesPoints(chartCtx, pair,
		chartWindow{from: from, gran: gran}, s.chartVWAPReader(gran, from))
	if errors.Is(err, ErrUnknownGranularity) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-granularity",
			"Invalid granularity", http.StatusBadRequest,
			// Enumeration comes from timescale.AllHistoryGranularities,
			// the same slice Validate ranges over — a hand-written copy
			// here would keep advertising the old set after a rung is
			// added or removed.
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
		wire[i] = HistoryPointWire{T: p.Bucket, P: p.VWAP, VUSD: p.VolumeUSD}
	}

	series := ChartSeries{
		AssetID:     pair.Base.String(),
		Quote:       pair.Quote.String(),
		Timeframe:   tfRaw,
		Granularity: gran,
		PriceType:   priceType,
		Points:      wire,
	}
	// Retention-truncation signal. We treat the response as truncated
	// when the consumer asked for a bounded window AND the earliest
	// returned bucket starts more than one granularity unit after
	// `from` — that's the difference between "the last 7 days are
	// flat" and "this deployment only has 7 days of data". R-013.
	//
	// `timeframe=all` (from.IsZero()) intentionally never trips the
	// flag — that timeframe explicitly means "everything you have",
	// so a short result IS the full result.
	if !from.IsZero() && len(points) > 0 {
		if grace := chartGranularityGrace(gran); points[0].Bucket.Sub(from) > grace {
			startsAt := points[0].Bucket
			requested := from
			series.Truncated = true
			series.DataStartsAt = &startsAt
			series.RequestedFrom = &requested
		}
	}

	s.writeChartSeries(w, r, pair, series, walk)
}

// dispatchSpecialisedChart routes to a non-default chart handler
// when the request matches a specialised shape: market_cap series,
// fiat:fiat pairs (which live in fx_quotes, not prices_1m), and
// price_type=twap (twap_1h / twap_1d CAGGs). Returns true when a
// specialised handler took the request (caller bails); false to let
// the default vwap path proceed.
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

// twapChartGranularity snaps an arbitrary requested chart granularity
// onto one of the two grains backed by a TWAP CAGG (migration 0081):
// sub-daily → 1h, daily+ → 1d. The TWAP surface is deliberately coarser
// than VWAP (which has all seven prices_* grains) — a 1h/1d TWAP is the
// meaningful resolution for a time-weighted view, and it keeps the CAGG
// footprint to two hierarchical views over prices_1m. handleChartTWAP
// reports the snapped grain back in the response so the consumer sees
// exactly what was served.
func twapChartGranularity(gran string) string {
	switch gran {
	case "1d", "1w", "1mo":
		return "1d"
	default: // 1m, 15m, 1h, 4h and any unknown → the finer TWAP grain
		return "1h"
	}
}

// ─── granularity fitting ────────────────────────────────────────

// chartVWAPGranularityLadder is the coarsening ladder the default
// price path walks: every served rung, finest first. It IS
// [timescale.AllHistoryGranularities] — the one declaration of the
// served set — rather than a second list, for the reason that slice's
// own doc gives: a hand-copied enumeration is how two lists of the
// same seven strings drift apart.
var chartVWAPGranularityLadder = timescale.AllHistoryGranularities

// chartTWAPGranularityLadder is the TWAP path's ladder, and it is
// deliberately NOT the served set. price_type=twap is backed by
// exactly two CAGGs (twap_1h / twap_1d, migration 0081), so
// [twapChartGranularity] snaps every request onto one of them and a
// coarsening that stepped past `1d` — or onto `4h` — would name a view
// that does not exist.
var chartTWAPGranularityLadder = []timescale.HistoryGranularity{
	timescale.Granularity1h, timescale.Granularity1d,
}

// chartGranularityFits reports whether a series at `gran` covering a
// window of `window` can be carried by one response — whether the
// grid `time_bucket(<gran>, …)` lays over that window has at most
// [historyMaxPoints] points.
//
// A grain with no known bucket width is not this function's to judge:
// it is a bad `?granularity=`, and the reader answers it with
// [ErrUnknownGranularity], which the handlers turn into the 400 that
// enumerates the served set. Reporting "fits" hands it straight
// through unchanged.
func chartGranularityFits(gran string, window time.Duration) bool {
	width := timescale.HistoryGranularity(gran).BucketDuration()
	if width <= 0 {
		return true
	}
	// CEILING, not floor: a window that spans 50,000.4 bucket widths
	// can still touch 50,001 grid points, and floor division would
	// call that a fit and hand back a series short by one bucket. No
	// timeframe lands on a fraction today (1y at 15m is exactly
	// 35,040), so this only fixes the direction of a future one.
	return (window+width-1)/width <= historyMaxPoints
}

// chartFitGranularity is the grain a window of `window` is SERVED at
// when `gran` was asked for: `gran` itself when its grid fits, else
// the finest COARSER rung of `ladder` that does.
//
// The reader caps every response at [historyMaxPoints] and the
// truncation takes the EARLIEST buckets, so a (timeframe,
// granularity) pair whose grid is wider than that cap cannot be
// answered as asked — and the surface answered it anyway. Measured on
// production 2026-09-07, `?timeframe=1y&granularity=1m` returned 200
// with 50,000 points covering 2026-05-05 to 2026-06-10: 36 of the 365
// days requested, ending three months before the request did, under a
// response that still said `granularity: "1m"`. None of the existing
// signals says that — `truncated` describes RETENTION at the series'
// start, `discontinuous` an interior hole, `stale` a source walk that
// was cut — so the wire carried a year-of-minutes shape with a month
// of stale minutes in it and nothing to tell the two apart.
//
// The response's own `granularity` carries the answer, and no new
// field is added, because that field already means "the grain this
// series is ON": the TWAP path has reported its snapped grain there
// since it shipped ([twapChartGranularity]), and a consumer plotting
// the array reads it to label the axis. A caller that sends `1m` and
// reads back `15m` knows precisely what happened; one that sends a
// pair that fits reads back what it sent.
//
// Coarsening is also what makes [chartWindow.covered] REACHABLE, which
// is the second half of the same defect: at 1y/1m the grid has
// ~525,600 points and the reader can never return more than 50,000, so
// the merge could not hold a full grid however complete the data was
// and the walk ran to [chartWalkBudget] on every request (measured
// 3.40-4.27s warm against 1.26-1.31s for the same window at 15m). The
// predicate is untouched — it was verified exact over 155 holed-set
// variants and stays that way; what changes is that the grid it
// measures against now fits under the cap.
//
// It is a property of the REQUEST alone — window width over bucket
// width — so it costs no read and cannot vary with how much data a
// pair happens to hold. That is also why it is applied ONLY where the
// window has a requested width. `timeframe=all` and
// /v1/history/since-inception ask for "everything you have", whose
// point count is a property of the DATA: measured the same day,
// `?timeframe=all&granularity=1h` serves 47,823 points spanning
// 2017-01-17 to now, complete and under the cap, because the pair's
// hourly buckets are sparse — while a grid laid from pubnet genesis
// would count 96,600 and coarsen a response that is already right.
// A `window <= 0` therefore returns `gran` untouched.
//
// A ladder that runs out returns `gran` as well. Unreachable with
// today's rungs (`1mo` fits any window under 4,000 years), and the
// point is the direction of the fallback: serving the requested grain
// truncated is what this surface already does, and is strictly better
// than naming a grain that was never read.
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

// handleChartTWAP serves /v1/chart?price_type=twap for a non-fiat base
// out of the twap_1h / twap_1d CAGGs (migration 0081). It mirrors the
// default VWAP path — closed CAGG buckets over the timeframe window,
// stablecoin-USD proxy fallback when the literal fiat:USD pair has no
// buckets — but reads the time-weighted series and snaps the
// granularity to the TWAP grain actually served.
func (s *Server) handleChartTWAP(
	w http.ResponseWriter,
	r *http.Request,
	pair canonical.Pair,
	tfRaw, gran string,
	from time.Time,
) {
	// Snap onto a TWAP-backed grain, then coarsen if even that grain's
	// grid outruns one response ([chartFitGranularity]). The second
	// step is a guard rather than a live path: the widest bounded
	// timeframe today is `1y`, which is 8,760 buckets at `1h`. It is
	// wired anyway because `chartTimeframes` advertises itself as a
	// one-line table to extend, and a one-line extension is exactly how
	// the defect would come back on the surface nobody re-checked.
	twapGran := chartFitGranularity(chartTWAPGranularityLadder,
		twapChartGranularity(gran), chartTimeframes[tfRaw].Duration)

	// 8s ceiling covering the CAGG scan + the proxy fallback retry,
	// matching the VWAP path (#1082 / #1099 …).
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	read := func(rc context.Context, p canonical.Pair) ([]HistoryPoint, error) {
		return s.history.TWAPPointsInRange(rc, p, twapGran, from, time.Time{}, historyMaxPoints)
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
		wire[i] = HistoryPointWire{T: p.Bucket, P: p.VWAP, VUSD: p.VolumeUSD}
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
			series.DataStartsAt = &startsAt
			series.RequestedFrom = &requested
		}
	}
	writeChartJSON(w, series, Flags{Triangulated: walk.proxied, Stale: walk.degraded})
}

// handleChartFiat serves /v1/chart for fiat:fiat pairs out of the
// fx_quotes hypertable. The Massive worker writes one row per ticker
// per UTC day into fx_quotes (the one-shot Frankfurter history backfill
// wrote the rows before it) — so any sub-daily
// granularity (1m / 15m / 1h / 4h) just gets the daily bar replicated
// to the consumer's chosen grain (front-end renders flat candles).
//
// Pair conventions:
//   - fiat:CCY/fiat:USD  → reader returns rate (1 CCY = N USD); use InverseUSD
//   - fiat:USD/fiat:CCY  → reader returns inverse (1 USD = N CCY); use RateUSD
//   - fiat:CCY1/fiat:CCY2 (cross, e.g. EUR/JPY) → triangulated on read
//     through both USD legs: price(base/quote) = rate_usd[quote] /
//     rate_usd[base] per daily bucket (rate_usd[T] = "1 USD = N T",
//     the same algebra /v1/price's tryFiatCrossRate uses). The
//     division runs in big.Rat, not float (ADR-0003 discipline for
//     the derived leg), and the response stamps flags.triangulated.
func (s *Server) handleChartFiat(
	w http.ResponseWriter,
	r *http.Request,
	pair canonical.Pair,
	tfRaw, gran, priceType string,
	from time.Time,
) {
	series := ChartSeries{
		AssetID:     pair.Base.String(),
		Quote:       pair.Quote.String(),
		Timeframe:   tfRaw,
		Granularity: gran,
		PriceType:   priceType,
		Points:      []HistoryPointWire{},
	}

	if s.fxHistory == nil {
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

	// Default window: trailing 1y when timeframe=all (open-ended would
	// hammer Postgres for 25y on every request; the chart consumer
	// only renders one screen anyway).
	to := time.Now().UTC().Truncate(24 * time.Hour)
	queryFrom := from
	if queryFrom.IsZero() {
		queryFrom = to.AddDate(-25, 0, 0) // ECB inception
	}

	fxCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	points, err := s.fxHistory.ListFXHistory(fxCtx, ticker, queryFrom, to)
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
		writeChartJSON(w, series, Flags{Stale: true})
		return
	}

	wire := make([]HistoryPointWire, 0, len(points))
	for _, p := range points {
		rate := p.RateUSD
		if useInverse {
			rate = p.InverseUSD
		}
		if rate <= 0 {
			continue
		}
		wire = append(wire, HistoryPointWire{
			T: p.Bucket,
			P: fmt.Sprintf("%.10f", rate),
			// FX rates have no volume — omit v_usd entirely.
		})
	}
	series.Points = wire

	// Retention-truncation signal — same shape as the crypto path.
	if !from.IsZero() && len(wire) > 0 {
		if grace := chartGranularityGrace(gran); wire[0].T.Sub(from) > grace {
			startsAt := wire[0].T
			requested := from
			series.Truncated = true
			series.DataStartsAt = &startsAt
			series.RequestedFrom = &requested
		}
	}

	writeChartJSON(w, series, Flags{})
}

// handleChartFiatCross serves /v1/chart for fiat:CCY1/fiat:CCY2 cross
// pairs (neither side USD) by triangulating both legs against USD out
// of fx_quotes: price(base/quote) on day d = rate_usd[quote] /
// rate_usd[base] — the same algebra /v1/price's tryFiatCrossRate
// applies to the live forex snapshot, here applied per historical
// bucket. Buckets are joined on equal date (both series are daily ECB
// reference rates); a day missing either leg is skipped rather than
// forward-filled, so every emitted point is two same-day observations.
// The division runs in big.Rat (exact on the given legs, ADR-0003
// discipline); the response stamps flags.triangulated.
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
	basePts, err := s.fxHistory.ListFXHistory(fxCtx, pair.Base.Code, queryFrom, to)
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
		writeChartJSON(w, series, Flags{Stale: true})
		return
	}
	quotePts, err := s.fxHistory.ListFXHistory(fxCtx, pair.Quote.Code, queryFrom, to)
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
		writeChartJSON(w, series, Flags{Stale: true})
		return
	}

	wire := crossFiatChartPoints(basePts, quotePts)
	series.Points = wire

	// Retention-truncation signal — same shape as the direct path.
	if !from.IsZero() && len(wire) > 0 {
		if grace := chartGranularityGrace(gran); wire[0].T.Sub(from) > grace {
			startsAt := wire[0].T
			requested := from
			series.Truncated = true
			series.DataStartsAt = &startsAt
			series.RequestedFrom = &requested
		}
	}
	writeChartJSON(w, series, Flags{Triangulated: len(wire) > 0})
}

// crossFiatChartPoints merges two ascending daily USD-leg series on
// equal buckets and emits the cross rate rate_usd[quote]/rate_usd[base]
// per shared day. big.Rat.SetFloat64 is exact for every finite float64,
// and the single Quo keeps the derived leg free of compounding float
// error; ratToDecimal renders the same 10-digit decimal string the
// other price surfaces use.
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
			if b.RateUSD <= 0 || q.RateUSD <= 0 {
				continue
			}
			br := new(big.Rat).SetFloat64(b.RateUSD)
			qr := new(big.Rat).SetFloat64(q.RateUSD)
			if br == nil || qr == nil || br.Sign() <= 0 {
				continue
			}
			cross := new(big.Rat).Quo(qr, br)
			wire = append(wire, HistoryPointWire{
				T: b.Bucket,
				P: ratToDecimal(cross, ohlcPriceDigits),
				// FX rates have no volume — omit v_usd entirely.
			})
		}
	}
	return wire
}

// chartGranularityGrace is the gap (in time) between `from` and the
// first returned bucket above which we consider the response
// truncated by retention. Picks one granularity period — anything
// less is "the first bucket happens to be empty"; anything more
// means the underlying CAGG simply doesn't have data going that far
// back. Unknown granularity strings fall through with a generous
// 1-day grace so we don't false-positive.
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

// chartBucketMerge accumulates a chart series bucket by bucket: the
// FIRST source offered a bucket owns it, and every later source is
// suppressed for that bucket AND FOR NO OTHER.
//
// That per-bucket rule is the one
// [docs/architecture/aggregate-alias-folding.md] §7.5 settled for the
// fiat OHLC series, applied at this surface's own grain. The chart used
// to resolve first-hit ONCE PER RESPONSE — the first source pair holding
// any bucket at all served the whole window — and §7.5's objection to
// that shape is a property rather than a preference: it makes the source
// set a function of the WINDOW, so the same bucket renders one way
// inside a window an earlier source also covers and another way inside
// one it does not, from one unchanged database. Resolving per bucket
// depends only on the bucket.
//
// Measured on the flagship pair 2026-09-06: `native/fiat:USD` at `1d`
// served 1,070 points with a 1,919-day break between 2021-01-31 and
// 2026-05-05, because `crypto:XLM/fiat:USD` answered first and won the
// whole response; 763 of those days sit in `<XLM SAC>/<USDC SAC>` — the
// same pool `/v1/ohlc` had already been serving since 2026-09-05 — and
// were never read. The proxy walk could reach that pool
// ([Server.chartFiatProxyPairs] enumerates it) and was gated on the
// series being EMPTY, which a series with a hole in it is not.
//
// Sources are NOT blended within a bucket. A bucket carries one venue
// set's own published aggregate, exactly as the CAGG wrote it, so this
// still never publishes a VWAP no venue set produced — the gate the
// /v1/vwap point path applies. What changes is only WHICH buckets are
// answered, never what an answered bucket says.
type chartBucketMerge struct {
	byBucket map[time.Time]HistoryPoint
}

func newChartBucketMerge() *chartBucketMerge {
	return &chartBucketMerge{byBucket: make(map[time.Time]HistoryPoint)}
}

// add claims every bucket of `points` that no earlier source claimed,
// reporting how many it took — which is how a caller learns whether a
// source contributed at all (and so whether the response is proxied).
//
// Buckets are keyed in UTC: [time.Time] compares its location as well as
// its instant, and two readers can hand back the same instant in two
// zones. Same reason [fiatPointGateBucket] normalises.
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

// series returns the merged buckets in ascending bucket order — the
// order every chart reader and [ChartSeries.markDiscontinuity] assume,
// and which map iteration does not give.
// earliest is the oldest claimed bucket; the zero time when nothing is
// claimed. [chartWindow.covered] anchors its grid walk on it.
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

// chartWindow is the request a source walk has to cover: the lower
// bound the read closure was built with, and the bucket grid it reads
// on. A zero `from` is a since-inception read — unbounded below, so
// coverage can never be established and the walk always runs its whole
// list.
type chartWindow struct {
	from time.Time
	gran string
}

// covered reports whether `m` already holds EVERY bucket the reader can
// return for this window, so no remaining source can add one and the
// walk may stop with a result identical to the full walk's.
//
// It is exact rather than heuristic, which is what makes the
// short-circuit invisible in the answer. The store returns only buckets
// at or after `from` and only buckets that have CLOSED, so the readable
// grid is bounded at both ends; every claimed bucket is a grid point (it
// came from `time_bucket`), so holding as many DISTINCT buckets as that
// grid has points means holding all of them — no set comparison needed.
//
// Two conditions, and the first is the subtle one: there must be no grid
// point between `from` and the earliest bucket held. That is tested as
// "the grid point one step BEFORE the earliest hit already lies before
// `from`", which needs no knowledge of the grid's origin and is exact at
// every phase, including a `from` that lands exactly on a boundary.
//
// Deliberately cheap — O(1) for the fixed-width grains, O(months) for
// `1mo` — because it runs before every read of a walk that may be 24
// pairs long.
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

// chartClosedBucketCount is how many buckets on the granularity's grid
// start at or after `first` and have CLOSED by `now` — the exact number
// of rows the reader could return for a window beginning at `first`.
// ok=false for a grain with no known grid.
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

// chartFixedBucketWidth returns the constant width of a granularity's
// bucket, and whether it HAS one. `1mo` does not: `time_bucket('1
// month', …)` lays down calendar months, so its width alternates
// between 28 and 31 days and only [chartBucketStep] can walk it.
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

// chartBucketPrev is [chartBucketStep] backwards: the start of the
// bucket immediately preceding the one starting at t. Safe for `1mo`
// because a monthly bucket start is always midnight on the 1st, where
// month arithmetic does not overflow into the following month.
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

// chartWalkBudget bounds the reads a source walk makes ONCE IT ALREADY
// HOLDS A SERIES TO SERVE. Reads taken while the merge is still empty
// are NOT bounded by it: those are the reads that produce the answer
// this surface served before the walk existed, and they keep the
// handler's own 8s ceiling and its error handling untouched.
//
// It exists because the walk multiplied the reads a populated request
// makes by up to 24 (3 alias spellings + 21 proxies), inside one 8s
// budget, and the reads are NOT cached — [CachedHistoryReader] wraps
// only LatestTradePerSource, so every HistoryPointsInRange is a fresh
// CAGG scan (handleChart's own comment records 5-10s for a cold one).
// Measured on production 2026-09-06 at `timeframe=1y&granularity=1m`,
// where each constituent returns the full historyMaxPoints cap:
// `native/USDC-GA5Z…` alone takes 8.112s and three other constituents
// sum to 5.867s, while the flagship `native/fiat:USD` serves in
// 1.09-1.39s. Unbounded, the walk turns that 200 into an 8s
// `503 chart-timeout`.
//
// 2s is chosen against those numbers: it is longer than a warm
// constituent read at any grain the defect actually lives at (a
// `1d`/`all` series is ~3k rows per pair), and short enough that the
// pathological fine-grain case abandons the walk after one read rather
// than spending the handler's whole ceiling. A walk that stops here
// serves what it merged and stamps flags.stale — never a 503.
const chartWalkBudget = 2 * time.Second

// chartWalkResult is what one source walk produced, beyond the points.
type chartWalkResult struct {
	// proxied is true when a served bucket came from a source whose
	// QUOTE leg is a proxy for the requested one → flags.triangulated.
	proxied bool
	// degraded is true when the walk stopped before its list was
	// exhausted — a read failed, or the budget ran out — so the series
	// may be shorter than the full walk would have produced →
	// flags.stale, which is exactly that flag's documented meaning
	// ("below this surface's documented baseline contract").
	degraded bool
}

// chartWalk is the state one source walk shares across its reads.
type chartWalk struct {
	s     *Server
	pair  canonical.Pair
	win   chartWindow
	read  func(context.Context, canonical.Pair) ([]HistoryPoint, error)
	merge *chartBucketMerge
	res   chartWalkResult
	spent time.Duration // time spent on reads taken after the merge filled
}

func (s *Server) newChartWalk(
	pair canonical.Pair, win chartWindow,
	read func(context.Context, canonical.Pair) ([]HistoryPoint, error),
) *chartWalk {
	return &chartWalk{s: s, pair: pair, win: win, read: read, merge: newChartBucketMerge()}
}

// chartSourceClass is what a source pair can do for the request, and it
// is the axis the walk's ERROR handling splits on.
//
//   - An ALIAS source is the requested pair in another canonical
//     spelling. It is the read this surface has always made, and its
//     failure IS the answer: [ErrUnknownGranularity] on a bad
//     `?granularity=`, a dead store, a cancelled client. Base
//     propagated it and so does this.
//   - A PROXY source is a peg or backer quote standing in for the
//     requested one. Base's own walk was `if err != nil || len(pp) == 0
//     { continue }` — skip the pair, try the next — and that is
//     restored here unconditionally. A proxy failure may never be the
//     answer.
type chartSourceClass int

const (
	chartSourceAlias chartSourceClass = iota
	chartSourceProxy
)

// step reads one source pair into the merge. It returns false when the
// walk must stop, and an error only on the one class of read whose
// failure is the answer.
//
// TWO axes, and conflating them is a regression in both directions:
//
//   - BUDGET is "can this read still ANSWER?", which is merge state. A
//     read taken while the merge is EMPTY keeps the handler's whole 8s
//     ceiling, because on this deployment it usually IS the answer: 59
//     of the 60 largest assets report flags.triangulated=true, meaning
//     the alias spellings hold nothing and a PROXY read carries the
//     entire series. Bounding that read would truncate the series for
//     almost every asset. Once the merge holds a series, a further read
//     can only FILL, and [chartWalkBudget] bounds it.
//   - ERRORS are "is this read's failure the ANSWER?", which is source
//     CLASS, not merge state. Splitting errors on merge state instead
//     inverted base behaviour exactly where the deployment lives: for
//     the 59-of-60, every proxy read runs on an empty merge, so a
//     failing early proxy turned base's `200` with the series into a
//     `503` with nothing — measured on a fixture whose series sits in
//     the peg's SAC form, first proxy timing out: base 200/5 points,
//     merge-state split 503/0 points.
//
// A proxy failure therefore CONTINUES to the next pair rather than
// stopping the walk, which is what base did and what finds the series
// when the failing pair is not the one holding it. When the failure was
// a budget timeout the continue is self-limiting: the next iteration
// sees the budget spent and stops.
func (w *chartWalk) step(ctx context.Context, sp canonical.Pair, class chartSourceClass) (bool, error) {
	if w.merge.empty() {
		// Nothing merged yet: this read can still be the answer, so it
		// keeps the handler's ceiling and, for an alias source, its
		// error handling.
		points, err := w.read(ctx, sp)
		if err != nil {
			if class == chartSourceAlias {
				return false, err
			}
			w.res.degraded = true
			return true, nil //nolint:nilerr // a proxy failure is never the answer — see above
		}
		w.claim(sp, points)
		return true, nil
	}
	if w.win.covered(w.merge, time.Now().UTC()) {
		return false, nil // every readable bucket is already claimed
	}
	remaining := chartWalkBudget - w.spent
	if remaining <= 0 {
		w.res.degraded = true
		return false, nil
	}
	rctx, cancel := context.WithTimeout(ctx, remaining)
	started := time.Now()
	points, err := w.read(rctx, sp)
	cancel()
	w.spent += time.Since(started)
	if err != nil {
		// The error is not dropped — it is CARRIED, as flags.stale on
		// the response, which is what this surface can honestly say
		// about a series that may be short. Returning it instead
		// discards every bucket already merged and answers 503, so a
		// single slow proxy destroys a complete series (measured: the
		// flagship at 1y/1m, 8.112s on one constituent). A LATER
		// spelling can still hold buckets this one does not, so the
		// walk continues; the budget check above ends it when the
		// failure was the budget itself.
		w.res.degraded = true
		return true, nil //nolint:nilerr // degrade the response, never fail it — see above
	}
	w.claim(sp, points)
	return true, nil
}

// claim normalises one source's points to true prices and offers them
// to the merge, recording whether the source's quote leg was a proxy.
func (w *chartWalk) claim(sp canonical.Pair, points []HistoryPoint) {
	points = w.s.adjustSourcePoints(sp, points)
	if w.merge.add(points) > 0 && !sp.Quote.Equal(w.pair.Quote) {
		w.res.proxied = true
	}
}

// adjustSourcePoints applies the dex-nonstandard-decimals forward
// normalisation with the SOURCE pair's own legs.
//
// It runs per source rather than once over the merged array because the
// merge can carry buckets from up to 24 source pairs whose quote assets
// differ (`fiat:USD`, a classic peg, that peg's SAC wrapper, four
// abstract backers…), and the correction factor is
// 10^(baseDecimals − quoteDecimals) of the pair the bucket was READ
// from. Applying the REQUESTED pair's factor to all of them scales
// buckets by a number derived from an asset that never appeared in
// them. It is a no-op wherever the two legs share a scale — which is
// every pair on this deployment today, so no served value moves — but
// it also corrects the pre-existing case, where a wholly proxied series
// was already being adjusted with the requested quote's decimals
// instead of the peg's.
func (s *Server) adjustSourcePoints(sp canonical.Pair, points []HistoryPoint) []HistoryPoint {
	return adjustHistoryPointPrices(points,
		aggregate.ResolveDecimals(s.nonstandardDecimals, sp.Base),
		aggregate.ResolveDecimals(s.nonstandardDecimals, sp.Quote))
}

// chartSeriesPoints is the whole read chain behind a CAGG-served chart
// series, and the one entry point every chart surface uses: the
// directly observed markets ([Server.chartObservedPoints]) and, only
// when those answered nothing at all, the series derived through XLM
// ([Server.fiatSeriesThroughXLM]). Returns the merged series and what
// the walk has to declare about it.
//
// The derived route stays a WHOLE-SERIES last resort rather than a
// per-bucket filler. Every observed source is one spelling of a market
// that traded, so §7.5's rule ranks them against each other; the cross
// is a value composed from two other markets, and mixing composed
// buckets into a traded series would put two kinds of number in one
// array with only a response-wide `flags.triangulated` to tell them
// apart. A wholly-derived series says so unambiguously, which is what
// it already did.
func (s *Server) chartSeriesPoints(
	ctx context.Context,
	pair canonical.Pair,
	win chartWindow,
	read func(context.Context, canonical.Pair) ([]HistoryPoint, error),
) ([]HistoryPoint, chartWalkResult, error) {
	points, res, err := s.chartObservedPoints(ctx, pair, win, read)
	if err != nil {
		return nil, chartWalkResult{}, err
	}
	if len(points) > 0 {
		return points, res, nil
	}
	if derived, ok, degraded := s.fiatSeriesThroughXLM(ctx, pair, win, read); ok {
		return derived, chartWalkResult{proxied: true, degraded: degraded}, nil
	}
	return nil, res, nil
}

// chartObservedPoints merges every DIRECTLY OBSERVED source of a chart
// series — the requested pair's alias spellings
// ([Server.chartMergeAliasPairs]) and then, for a fiat quote, the
// stablecoin proxies ([Server.chartStablecoinFallback]) — into one
// per-bucket series. No derivation: it is what a leg of
// [Server.fiatSeriesThroughXLM] may read without the cross recursing
// into itself.
func (s *Server) chartObservedPoints(
	ctx context.Context,
	pair canonical.Pair,
	win chartWindow,
	read func(context.Context, canonical.Pair) ([]HistoryPoint, error),
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

// chartStablecoinFallback handles the X/fiat → X/<proxy> retry path.
// The literal fiat-quoted pair rarely has rows in the CAGGs because
// the stablecoin → fiat mapping is aggregator policy applied at read
// time, not at write time — the depth lives under the stablecoin and
// classic-peg pairs. It walks the proxy source pairs (see
// [Server.chartFiatProxyPairs]) and fills the buckets the requested
// quote's own spellings left unanswered. A non-fiat quote has no
// proxies and is a no-op.
//
// It runs on EVERY fiat-quoted request, not only on an empty series.
// Gating it on emptiness is what hid five years of the flagship pair:
// a series with a hole in it is not empty, so the walk that could fill
// the hole never ran. The per-bucket claim in [chartBucketMerge] is
// what makes running it unconditionally safe — a proxy cannot displace
// a bucket the requested quote answered, so a series that was complete
// before is byte-identical after.
//
// What it must never do is COST that series, and what it must never
// become is the answer's failure. A read here that arrives once the
// merge already holds a series is bounded by [chartWalkBudget]; a read
// here that FAILS, at any merge state, marks the response degraded and
// moves to the next pair, which is base's own `if err != nil ||
// len(pp) == 0 { continue }` — load-bearing on this deployment, where
// 59 of the 60 largest assets are carried entirely by a proxy read and
// every proxy read therefore runs on an empty merge. The walk also
// stops outright once the requested window is fully claimed
// ([chartWindow.covered]), so a populated request pays for the reads
// that can still add something and no more.
//
// `read` fetches one pair's closed-bucket series — the VWAP path
// passes a prices_<gran> reader, the TWAP path a twap_<gran> reader —
// so both CAGG-reading chart surfaces share the same fallback chain.
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

// chartFiatProxyPairs is the ordered proxy-source list a fiat-quoted
// chart series is filled from. It is the chart's analogue of the
// constituent set the live aggregator's VWAP and the OHLC-series path
// ([Server.ohlcSeriesFiatCombined]) combine — the earlier
// classic-pegs-only form (BACKLOG #37 gap) missed the abstract
// stablecoin backers, so a chart for a pair whose USD depth is
// CEX-sourced (crypto:XLM/crypto:USDT, from binance) found nothing.
//
// Order is deterministic for cross-region stability (ADR-0015), and it
// is load-bearing rather than cosmetic: under [chartBucketMerge] the
// first pair holding a bucket owns it, so this list is the priority
// with which sources are ranked against each other, bucket by bucket.
// Two passes across ALL peg families, not one pass per family:
//
//  1. the ESTABLISHED quote spellings — each operator USD-pegged
//     classic in its priority-first (classic) form, in config order,
//     then the abstract stablecoin backers pegged to the quote's fiat
//     (crypto:USDT / crypto:USDC / … — sorted; EUR-quoted charts reach
//     crypto:EURC etc. via aggregate.FiatBackers);
//  2. the HELD-BACK spellings — the remaining canonical forms of those
//     same pegs, in practice a declared peg's SAC wrapper, which is
//     where every Soroban AMM's dollar leg is stored.
//
// The two-pass split is [Server.usdPegProxyQuotes]'s classic-then-SAC
// rule lifted from the quote asset to the whole list, and it is the
// same split [Server.usdPeggedConstituentSets] gives the OHLC series.
// It matters because a pool is routinely orders of magnitude thinner
// than the book of the same family: a per-family ordering would let one
// family's SAC pool own a bucket another family's classic book can
// answer, which is the +37.32% bar
// [docs/architecture/aggregate-alias-folding.md] §7.5 measured. Ranking
// every established spelling ahead of every held-back one keeps a
// held-back source to the buckets where the alternative is nothing.
//
// The base leg keeps its own ordering inside each pass
// ([canonical.AssetAliases] — SAC last), so the split is on the QUOTE
// leg only, exactly as §7.5 defines it.
//
// Each proxy quote is crossed with every base alias. The literal pair
// the alias walk already read is skipped; duplicates are dropped, first
// occurrence kept; and a combination whose two sides are one asset in
// two spellings (sameAsset) is dropped rather than read, since it can
// never be a market.
func (s *Server) chartFiatProxyPairs(pair canonical.Pair) []canonical.Pair {
	established, heldBack := s.chartFiatProxyQuotes(pair.Quote)
	out := s.chartProxyPairsFor(pair, established, nil)
	return s.chartProxyPairsFor(pair, heldBack, out)
}

// chartFiatProxyQuotes splits the proxy quote assets of a fiat quote
// into the established spellings and the held-back ones — see
// [Server.chartFiatProxyPairs] for what the split is for. Their union is
// [Server.usdPegProxyQuotes] plus the fiat's abstract backers, which is
// the set the chart has always read; only the order changes.
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
		for _, peg := range s.usdPeggedClassics {
			add(&established, canonical.CanonicalAsset(peg))
		}
	}
	// (1b) abstract stablecoin backers for the quote's fiat, sorted.
	backers := aggregate.FiatBackers(quote.Code)
	sort.Strings(backers)
	for _, code := range backers {
		if a, err := canonical.NewCryptoAsset(code); err == nil {
			add(&established, a)
		}
	}
	// (2) the remaining canonical forms of each declared peg — the SAC
	// wrappers — after every established spelling of every family.
	if quote.Code == "USD" {
		for _, peg := range s.usdPeggedClassics {
			for _, form := range assetAliases(peg) {
				add(&heldBack, form)
			}
		}
	}
	return established, heldBack
}

// chartProxyPairsFor crosses one pass of proxy quotes with every base
// alias, appending to `out` and keeping its dedupe — so the established
// pass and the held-back pass share one `seen` set and a market reached
// by both stays with the pass read first.
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

// chartAliasPairs is the requested pair in every canonical spelling of
// both its legs — the XLM dual-form cross (F-1340) — in
// [canonical.AssetAliases] priority order, so the literal form leads and
// the SAC forms trail. Degenerate combinations (one asset against
// itself) are dropped rather than read.
//
// Separated from the read so [Server.chartCoverageSet] can enumerate
// exactly what the serving read enumerates, from one definition.
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

// chartMergeAliasPairs reads every alias spelling of the requested pair
// into `m`, so each bucket is served by the highest-priority spelling
// that holds it.
//
// The literal-keyed read alone left every chart surface blind to the
// venues publishing XLM under the other id: `?asset=native` read only
// native/<quote> buckets while the CEX-fed series lives under
// `crypto:XLM/<quote>`. [Server.chartStablecoinFallback] did not cover
// that gap — it crosses the base aliases with PROXY quotes only and
// skips the requested quote, so the one pair holding the answer
// (`crypto:XLM/fiat:USD`) was the one combination never read, and a
// chart that did fall through to a peg was needlessly stamped
// triangulated.
//
// An alias form is the same asset in another canonical spelling, not a
// proxy, so a hit here does NOT raise flags.triangulated — the same
// distinction [Server.fiatCombinedTrades] draws. Spellings are ranked
// against each other per BUCKET rather than blended: blending would
// publish a VWAP no venue set produced, exactly the gate the /v1/vwap
// point path applies, so an answered bucket still carries one CAGG's own
// aggregate byte for byte.
//
// An alias spelling's error (e.g. [ErrUnknownGranularity], which is
// form-invariant) propagates unchanged: this is the read whose failure
// IS the answer, exactly as it was before the walk existed. Only once a
// spelling has already answered does a later one's failure degrade the
// response instead — see [chartSourceClass] for the two axes.
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
// fetches a pair's closed prices_<gran> series over [from, now).
func (s *Server) chartVWAPReader(gran string, from time.Time) func(context.Context, canonical.Pair) ([]HistoryPoint, error) {
	return func(ctx context.Context, p canonical.Pair) ([]HistoryPoint, error) {
		return s.history.HistoryPointsInRange(ctx, p, gran, from, time.Time{}, historyMaxPoints)
	}
}

// fiatSeriesThroughXLM derives a fiat-quoted series for a non-XLM asset
// by crossing its XLM-quoted series with XLM's own series in that fiat,
// bucket by bucket:
//
//	price(asset, CCY)[t] = price(asset, XLM)[t] × price(XLM, CCY)[t]
//
// It is analogous to the USD-anchored point derivation (ADR-0051,
// tryUSDAnchoredFiatCross) but pivots through XLM, not USD — the USD
// peg's own USD series has no USD leg to anchor on — and is deliberately the
// LAST route the fiat fallback tries: every directly observed market —
// the literal pair, its alias spellings, the declared-peg proxies and
// the abstract backers — has already come back empty by the time this
// runs, so it can only ever fill a series that was otherwise absent,
// never displace one. The response carries flags.triangulated=true
// because the value is composed, not traded.
//
// The route exists because the proxy walk cannot price the numeraire
// itself. Every USD series on chain is served by rewriting the quote to
// a declared peg, and the declared peg (Circle USDC on this deployment)
// has no USD-quoted buckets under any of its spellings, at any grain:
// its dollar depth is the USDC/XLM book on SDEX and the USDC-SAC/XLM-SAC
// pools on Soroban. Crossing that book with XLM's CEX-quoted dollar
// series is the peg's actual traded dollar price — the surface where a
// depeg is visible — which a flat 1.0 asserted from the peg declaration
// would not be, and which is why the declaration is not synthesised
// backwards into a series here.
//
// Both legs are read through [Server.chartObservedPoints] — the same
// per-bucket merge over alias spellings and stablecoin proxies the
// requested pair itself gets, minus this derivation, so a leg cannot
// recurse into a cross of crosses. The asset leg therefore reaches the
// SAC-quoted Soroban pools (asset-SAC/XLM-SAC) and the pivot leg
// reaches the CEX series stored under `crypto:XLM` AND the pool buckets
// that CEX series does not hold: a pivot with a five-year hole in it
// would punch that hole through into every series derived from it, which
// is the defect this surface is being repaired for, one level down. The
// asset leg is read first so an asset with no XLM market at all — the
// common miss — costs no pivot read. Only buckets present on BOTH legs
// are emitted; a leg the reader truncated at its row cap yields the
// overlap, never a mismatched product. Base-side buckets carry the asset's own USD volume, which is
// what the derived series reports. An XLM base (any spelling) and a fiat
// base are not crossed: the former is the anchor itself and was already
// read literally, the latter is fx_quotes' surface.
func (s *Server) fiatSeriesThroughXLM(
	ctx context.Context, pair canonical.Pair, win chartWindow,
	read func(context.Context, canonical.Pair) ([]HistoryPoint, error),
) ([]HistoryPoint, bool, bool) {
	legs, ok := fiatCrossLegsThroughXLM(pair)
	if !ok {
		return nil, false, false
	}
	assetPts, assetRes, err := s.chartObservedPoints(ctx, legs[0], win, read)
	if err != nil || len(assetPts) == 0 {
		return nil, false, false
	}
	xlmPts, xlmRes, err := s.chartObservedPoints(ctx, legs[1], win, read)
	if err != nil || len(xlmPts) == 0 {
		return nil, false, false
	}
	crossed := crossSeriesThroughPivot(assetPts, xlmPts)
	// A leg that stopped short makes the PRODUCT short: the cross emits
	// only buckets present on both, so a truncated leg silently trims
	// the derived series. Carry the degradation out rather than letting
	// it vanish between the two reads.
	return crossed, len(crossed) > 0, assetRes.degraded || xlmRes.degraded
}

// fiatCrossLegsThroughXLM is the gate and the leg enumeration of
// [Server.fiatSeriesThroughXLM] on its own: for a fiat-quoted, non-fiat,
// non-XLM base it returns the asset leg (base/XLM) and the pivot leg
// (XLM/quote) the derivation multiplies, and ok=false for every pair
// the route does not apply to. It is a separate function so the
// coverage floor ([Server.chartCoverageSet]) enumerates the SAME legs
// the serving read multiplies, from one definition.
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

// crossSeriesThroughPivot merges two ascending closed-bucket series on
// equal buckets and emits base/pivot × pivot/quote per shared bucket —
// the same merge-join [crossFiatChartPoints] runs for fiat legs, here on
// the NUMERIC text the CAGGs serve. The product is [crossThroughPivot]'s:
// one exact big.Rat multiplication (ADR-0003: no float on the value
// path) rendered to the 10 fractional digits the other derived price
// surfaces use. A bucket missing on either side is skipped rather than
// carried forward; a leg that fails to parse or is not strictly positive
// is skipped for that bucket, since a price is only defined for positive
// rates. VolumeUSD is the base leg's: the asset's own traded USD volume
// in that bucket, unchanged by the pivot.
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
			})
		}
	}
	return out
}

// crossThroughPivot is the one multiplication under every pivot cross on
// the price surfaces — base/pivot × pivot/quote = base/quote — on the
// NUMERIC text the readers serve. Exact big.Rat (ADR-0003: no float on
// the value path), rendered with [ratToDecimal] to the 10 fractional
// digits the other derived price surfaces use. ok=false when either leg
// fails to parse or is not strictly positive: a price is only defined
// for positive rates, so a zero, negative or missing leg is a miss, not
// a zero. [crossSeriesThroughPivot] applies it per shared bucket for the
// series surfaces; [Server.crossDeclaredPegThroughXLM] applies it once
// for the point surface, so the two cannot drift apart in rounding or in
// what they refuse.
func crossThroughPivot(basePerPivot, pivotPerQuote string) (string, bool) {
	br, pr := ratFromDecimal(basePerPivot), ratFromDecimal(pivotPerQuote)
	if br == nil || pr == nil || br.Sign() <= 0 || pr.Sign() <= 0 {
		return "", false
	}
	return ratToDecimal(new(big.Rat).Mul(br, pr), ohlcPriceDigits), true
}

// adjustHistoryPointPrices applies the dex-nonstandard-decimals forward
// normalization to every point's VWAP field — see the call sites in
// handleChart / handleChartTWAP / handleChartMarketCapCrypto for the full
// rationale (docs/operations/runbooks/dex-nonstandard-decimals.md).
//
// VolumeUSD is intentionally NOT touched — prices_<gran>'s volume_usd
// column is already USD-denominated (Σ usd_volume, computed upstream at
// trade-valuation time), invariant to the base/quote decimals split. Only
// the raw quote/base price ratio needs the correction.
//
// Returns points UNCHANGED (same slice, no allocation) when
// baseDecimals == quoteDecimals — every pair without a confirmed
// non-7-decimals leg. This matters for byte-identical wire output: the
// CAGG's raw NUMERIC::text formatting doesn't match [ratToDecimal]'s
// fixed 10-digit rendering, so reformatting unconditionally would change
// the wire bytes for every already-correct 7dp pair — the overwhelming
// common case.
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

// parseChartParams resolves timeframe, granularity, and price_type
// — applying ADR-0020 defaults and rejecting unsupported values.
// Returns (raw timeframe, timeframe spec, granularity, price_type,
// ok). ok=false on any validation failure (problem written).
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

// parseChartAssetQuote pulls `asset` (required) + `quote` (default
// fiat:USD per defaultPriceQuote) from the chart request. Returns
// ok=false after writing a problem response on any parse error.
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

// handleChartMarketCap serves /v1/chart?price_type=market_cap.
//
// Fiat base (asset=fiat:CNY&quote=fiat:USD): daily series = M2
// (verified-currency catalogue) × inverse_usd (fx_quotes daily
// snapshot of 1 CCY → N USD).
//
// Non-fiat (on-chain) base: routed to handleChartMarketCapCrypto —
// daily USD price × daily circulating supply (supply_1d CAGG,
// migration 0066).
//
// The quote is always fiat:USD (market cap is USD-denominated).
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

	if s.verifiedCurrencies == nil || s.fxHistory == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/market-cap-unavailable",
			"market_cap not configured", http.StatusServiceUnavailable,
			"this deployment hasn't wired the verified-currency catalogue and/or fx_quotes reader")
		return
	}

	vc, ok := s.verifiedCurrencies.LookupByTicker(pair.Base.Code)
	if !ok || vc.CirculatingSupply == "" {
		writeChartJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{})
		return
	}
	// Exact circulating supply in whole units (INV-2 / ADR-0003 — the
	// catalogue carries supply as an exact decimal STRING; parsing it
	// to float64 truncates once it exceeds a float's 53-bit mantissa,
	// e.g. a quadrillion-unit fiat M2). Scale by supply_decimals via
	// big.Rat, never float division.
	m2, ok := fiatSupplyWholeUnits(vc.CirculatingSupply, vc.SupplyDecimals)
	if !ok {
		s.logger.Warn("market_cap: bad catalogue supply",
			"ticker", vc.Ticker, "supply", vc.CirculatingSupply)
		writeChartJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{})
		return
	}

	// Default window: trailing 1y when timeframe=all (open-ended
	// would hammer Postgres + the catalogue M2 doesn't change over
	// time anyway, so 25y of "same number × per-day FX" is just
	// noise).
	to := time.Now().UTC().Truncate(24 * time.Hour)
	queryFrom := from
	if queryFrom.IsZero() {
		queryFrom = to.AddDate(-25, 0, 0)
	}

	fxCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	points, err := s.fxHistory.ListFXHistory(fxCtx, pair.Base.Code, queryFrom, to)
	if err != nil {
		s.marketCapReadFailed(w, r, fxCtx, err, "ListFXHistory", pair, tfRaw, gran, from, "market_cap: fx_quotes fetch failed", "ticker", pair.Base.Code, "err", err)
		return
	}

	wire := make([]HistoryPointWire, 0, len(points))
	for _, p := range points {
		if p.InverseUSD <= 0 {
			continue
		}
		// market_cap = supply × rate, exact big.Rat. The rate is a
		// float64 from the FX feed; convert via its shortest round-trip
		// decimal so the multiplication itself introduces no float
		// rounding (only the source rate's own precision, which the
		// crypto path's usdMarketValue shares).
		rate, ok := new(big.Rat).SetString(strconv.FormatFloat(p.InverseUSD, 'f', -1, 64))
		if !ok {
			continue
		}
		wire = append(wire, HistoryPointWire{
			T: p.Bucket,
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
		if grace := chartGranularityGrace(gran); wire[0].T.Sub(from) > grace {
			startsAt := wire[0].T
			requested := from
			series.Truncated = true
			series.DataStartsAt = &startsAt
			series.RequestedFrom = &requested
		}
	}
	writeChartJSON(w, series, Flags{})
}

// writeChartTimeout answers a chart read that blew its own budget while
// the request was still live. The fiat and fiat-cross legs used to fall
// through to an empty series at 200 with no flag — the same shape the
// market-cap leg had — and a deadline is retryable capacity, not an
// absence of data.
func (s *Server) writeChartTimeout(w http.ResponseWriter, r *http.Request, leg, ticker string) {
	w.Header().Set("Retry-After", "5")
	writeProblem(w, r, "https://api.stellarindex.io/errors/chart-timeout", "Chart query timed out",
		http.StatusServiceUnavailable,
		fmt.Sprintf("%s for %s did not return inside the request budget; retry shortly.", leg, ticker))
}

// marketCapReadFailed triages a failed market-cap leg read. A client hangup
// writes nothing; a blown budget is retryable capacity and gets the
// chart-timeout 503; anything else degrades to an empty series that is
// flagged stale rather than presented as fact.
func (s *Server) marketCapReadFailed(w http.ResponseWriter, r *http.Request, ctx context.Context, err error, leg string, pair canonical.Pair, tfRaw, gran string, from time.Time, msg string, kv ...any) {
	if clientAborted(r, err) {
		return
	}
	if handlerTimedOut(ctx, err) {
		s.writeMarketCapTimeout(w, r, leg, pair, tfRaw, gran)
		return
	}
	s.logger.Warn(msg, append(kv, "err", err)...)
	writeChartJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{Stale: true})
}

// writeMarketCapTimeout answers a market-cap read that blew its
// deadline with the same `chart-timeout` 503 the vwap and twap chart
// paths already use.
//
// The market-cap legs used to degrade to emptyMarketCapSeries at 200 on
// ANY read error, deadline included. An empty series is a syntactically
// valid answer, so a caller renders "market cap $0" for an asset with
// real supply and has no way to tell that from a genuine no-data
// window — the same wrong-answer-with-full-confidence failure as the
// bodyless 200 this endpoint's own budget was supposed to prevent. A
// deadline is retryable, and only a 5xx says so.
//
// Both legs share one writer because both are the same statement to the
// caller ("this series is unavailable right now, retry"); which leg blew
// is a server-side detail, carried in the log line.
func (s *Server) writeMarketCapTimeout(w http.ResponseWriter, r *http.Request, leg string, pair canonical.Pair, tfRaw, gran string) {
	s.logger.Warn("market_cap crypto: deadline exceeded",
		"leg", leg, "asset", pair.Base.String(), "quote", pair.Quote.String(),
		"timeframe", tfRaw, "granularity", gran)
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/chart-timeout",
		"Chart query timed out", http.StatusServiceUnavailable,
		"the market-cap series' price + supply reads didn't both return inside the request budget; retry shortly.")
}

// emptyMarketCapSeries is the no-data response shape used when the
// catalogue doesn't carry a supply for the asset or the FX feed has
// no rows for the requested window. Keeping it as a helper means
// every error path emits the same wire shape (empty points array,
// not null).
//
// Callers reaching it from a read FAILURE must flag the envelope
// Stale — an unflagged empty series claims "this asset has no market
// cap", which is a different fact from "we could not compute one".
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

// handleChartMarketCapCrypto serves /v1/chart?price_type=market_cap
// for an on-chain (native / classic / Soroban) base. Market cap is a
// daily series: each day's USD price × that day's circulating supply.
//
//   - USD price: the existing daily price series the normal chart
//     serves (prices_1d, with the stablecoin-USD proxy fallback for
//     the common case where nothing trades directly in fiat:USD).
//   - circulating supply: the supply_1d CAGG (migration 0066),
//     forward-filled so a day with a price but no fresh supply
//     snapshot still gets the most-recent known supply.
//
// Off-chain crypto:* reference assets (BTC/ETH/…) have no on-chain
// supply we publish (supply.AssetKey errors), so they return an empty
// series rather than a fabricated cap.
func (s *Server) handleChartMarketCapCrypto(
	w http.ResponseWriter,
	r *http.Request,
	pair canonical.Pair,
	tfRaw string,
	from time.Time,
) {
	const gran = "1d" // market cap is always a daily series

	if s.history == nil || s.supply == nil {
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
		chartWindow{from: from, gran: gran}, s.chartVWAPReader(gran, from))
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
		writeChartJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{Stale: true})
		return
	}
	// The price leg is already normalised per SOURCE pair inside the walk
	// ([Server.adjustSourcePoints]) — it has to be, since the merged
	// array can carry buckets read under several different quote assets.
	// baseDec is still needed here for the SUPPLY leg (M2): circulating
	// supply is denominated in the base token's own smallest unit, so a
	// confirmed non-7-decimals token's supply must be divided by
	// 10^baseDec — not a hardcoded 10^7 — or the cap is off by
	// 10^(baseDec−7). Price and supply share the same baseDec, so
	// market_cap = supply × price stays internally coherent.
	//
	// LATENT, unreachable today, recorded rather than fixed here:
	// [NonstandardDecimalsCache.Lookup] is a raw map lookup on the exact
	// asset id and does NOT fold aliases, while the price leg above is
	// now scaled per SOURCE pair — whose base may be a different
	// spelling of this same asset. If the table ever names one spelling
	// of an aliasing family and not another, the price buckets read
	// under the SAC base and this supply division by the classic base's
	// decimals would diverge by a power of ten. It cannot happen on this
	// deployment: all five rows are bare C… contract ids whose alias
	// families are singletons. The durable fix is alias-folding the
	// lookup, which is a change to the cache's own contract and belongs
	// with its other callers, not here.
	baseDec := aggregate.ResolveDecimals(s.nonstandardDecimals, pair.Base)

	// Daily circulating supply (forward-filled via the carry-in row).
	to := time.Now().UTC().Truncate(24 * time.Hour)
	supPts, err := s.supply.DailyCirculatingSupply(ctx, supplyKey, from, to)
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
		writeChartJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{Stale: true})
		return
	}

	wire := marketCapPoints(pricePts, supPts, baseDec)
	series := ChartSeries{
		AssetID:     pair.Base.String(),
		Quote:       pair.Quote.String(),
		Timeframe:   tfRaw,
		Granularity: gran,
		PriceType:   "market_cap",
		Points:      wire,
	}
	if !from.IsZero() && len(wire) > 0 {
		if grace := chartGranularityGrace(gran); wire[0].T.Sub(from) > grace {
			startsAt := wire[0].T
			requested := from
			series.Truncated = true
			series.DataStartsAt = &startsAt
			series.RequestedFrom = &requested
		}
	}
	writeChartJSON(w, series, Flags{Triangulated: walk.proxied, Stale: walk.degraded})
}

// marketCapPoints forward-fills daily supply onto the daily USD-price
// series and multiplies: each price day gets the most-recent
// circulating supply at-or-before that day. Both inputs are ascending
// by bucket; a single forward cursor over supPts keeps it O(n+m). A
// price day with no supply at-or-before it (asset priced before its
// first supply snapshot) is skipped rather than emitted as zero.
func marketCapPoints(pricePts []HistoryPoint, supPts []timescale.SupplyDayPoint, baseDecimals int) []HistoryPointWire {
	wire := make([]HistoryPointWire, 0, len(pricePts))
	si := 0
	var cur *big.Int
	for _, pp := range pricePts {
		for si < len(supPts) && !supPts[si].Bucket.After(pp.Bucket) {
			cur = supPts[si].Circulating
			si++
		}
		if cur == nil || pp.VWAP == "" {
			continue
		}
		mc, err := usdMarketValue(cur, pp.VWAP, baseDecimals)
		if err != nil {
			continue
		}
		wire = append(wire, HistoryPointWire{T: pp.Bucket, P: mc})
	}
	return wire
}

// fiatSupplyWholeUnits converts the catalogue's (supply, decimals)
// tuple into an EXACT whole-unit big.Rat. The catalogue stores
// supplies as decimal strings in the asset's smallest integer unit
// (per the seed.yaml convention), alongside a decimals exponent. For
// fiat M2 the decimals are 0 so the supply is already in major units
// (e.g. "21700000000000" = $21.7T); for tokens decimals would be
// 7 / 18 / etc, and we divide by 10^decimals via big.Rat.
//
// Exact by construction (INV-2 / ADR-0003): the earlier float64 form
// truncated any supply past a float's 53-bit mantissa (~9.0e15) — a
// real risk for high-denomination fiat M2 figures. Returns ok=false
// when supplyStr isn't a decimal or decimals is negative.
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
