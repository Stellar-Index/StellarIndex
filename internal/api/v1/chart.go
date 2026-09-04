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

	// 8s ceiling on the chart query + downstream stablecoin
	// fallback. Same pattern as #1082 / #1099 / #1100 / #1101.
	// The chart's prices_1m / prices_5m / prices_1h scan can take
	// 5–10s on a cold cache for long timeframes (`?timeframe=1y`
	// + `granularity=1h` is ~8 760 buckets).
	chartCtx, chartCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer chartCancel()
	points, err := s.chartPointsWithAliases(chartCtx, pair, s.chartVWAPReader(gran, from))
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

	triangulated := false
	if len(points) == 0 {
		// Stablecoin fallback inherits chartCtx so the 8s ceiling
		// covers the proxy retry too — without that, an empty
		// literal pair could spend another 8s on each pegged
		// alternative (10+ pegs × 8s each).
		if fp, ok := s.chartStablecoinFallback(chartCtx, pair, s.chartVWAPReader(gran, from)); ok {
			points = fp
			triangulated = true
		}
	}

	// dex-nonstandard-decimals forward normalization (2026-07-10, closing
	// the deferred CAGG-reading tail from docs/operations/runbooks/
	// dex-nonstandard-decimals.md): /v1/chart was never guarded at all —
	// it read the same raw prices_<gran> CAGG ratio /v1/price's
	// closed-1m-bucket path does, just at coarser grains. See
	// adjustHistoryPointPrices for the byte-identical-on-7dp contract.
	baseDec := aggregate.ResolveDecimals(s.nonstandardDecimals, pair.Base)
	quoteDec := aggregate.ResolveDecimals(s.nonstandardDecimals, pair.Quote)
	points = adjustHistoryPointPrices(points, baseDec, quoteDec)

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

	s.writeChartSeries(w, r, pair, series, triangulated)
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
	twapGran := twapChartGranularity(gran)

	// 8s ceiling covering the CAGG scan + the proxy fallback retry,
	// matching the VWAP path (#1082 / #1099 …).
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	read := func(rc context.Context, p canonical.Pair) ([]HistoryPoint, error) {
		return s.history.TWAPPointsInRange(rc, p, twapGran, from, time.Time{}, historyMaxPoints)
	}

	points, err := s.chartPointsWithAliases(ctx, pair, read)
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

	triangulated := false
	if len(points) == 0 {
		if fp, ok := s.chartStablecoinFallback(ctx, pair, read); ok {
			points = fp
			triangulated = true
		}
	}

	// dex-nonstandard-decimals forward normalization — see handleChart's
	// equivalent comment. twap_1h/twap_1d carry the same raw
	// quote/base ratio shape as prices_<gran>.
	baseDec := aggregate.ResolveDecimals(s.nonstandardDecimals, pair.Base)
	quoteDec := aggregate.ResolveDecimals(s.nonstandardDecimals, pair.Quote)
	points = adjustHistoryPointPrices(points, baseDec, quoteDec)

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
	writeJSON(w, series, Flags{Triangulated: triangulated})
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
		writeJSON(w, series, Flags{})
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
		writeJSON(w, series, Flags{Stale: true})
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

	writeJSON(w, series, Flags{})
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
		writeJSON(w, series, Flags{Stale: true})
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
		writeJSON(w, series, Flags{Stale: true})
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
	writeJSON(w, series, Flags{Triangulated: len(wire) > 0})
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

// chartStablecoinFallback handles the X/fiat → X/<proxy> retry path.
// The literal fiat-quoted pair rarely has rows in the CAGGs because
// the stablecoin → fiat mapping is aggregator policy applied at read
// time, not at write time — the depth lives under the stablecoin and
// classic-peg pairs. When the literal pair returned 0 points and the
// quote is fiat, walk the proxy source pairs (see
// [Server.chartFiatProxyPairs]) and return the first non-empty result.
// When no proxy answers either, the series is derived through XLM
// ([Server.fiatSeriesThroughXLM]) — strictly last, so a directly
// observed market always wins over a derived one. ok=false when no
// fallback fires (caller keeps the empty result + leaves
// triangulated=false).
//
// `read` fetches one pair's closed-bucket series — the VWAP path
// passes a prices_<gran> reader, the TWAP path a twap_<gran> reader —
// so both CAGG-reading chart surfaces share the same fallback chain.
//
// Extracted to keep handleChart under the gocognit ceiling.
func (s *Server) chartStablecoinFallback(
	ctx context.Context, pair canonical.Pair,
	read func(context.Context, canonical.Pair) ([]HistoryPoint, error),
) ([]HistoryPoint, bool) {
	if pair.Quote.Type != canonical.AssetFiat {
		return nil, false
	}
	for _, proxied := range s.chartFiatProxyPairs(pair) {
		pp, err := read(ctx, proxied)
		if err != nil || len(pp) == 0 {
			continue
		}
		return pp, true
	}
	return s.fiatSeriesThroughXLM(ctx, pair, read)
}

// chartFiatProxyPairs is the ordered proxy-source list to try when a
// fiat-quoted chart series has no direct rows. It is the chart's
// first-hit analogue of the constituent set the live aggregator's VWAP
// and the OHLC-series path (ohlcSeriesFiatCombined) combine — the
// earlier classic-pegs-only form (BACKLOG #37 gap) missed the abstract
// stablecoin backers, so a chart for a pair whose USD depth is
// CEX-sourced (crypto:XLM/crypto:USDT, from binance) found nothing.
//
// Order is deterministic for cross-region stability (ADR-0015) and
// preserves the legacy preference:
//  1. operator USD-pegged classics (config order), then the same pegs'
//     other canonical forms — the SAC wrappers — in that order
//     ([Server.usdPegProxyQuotes]): keeps classic Circle USDC winning
//     where it has data, and reaches the Soroban pools that quote the
//     same peg as its SAC only once every classic peg came back empty;
//  2. abstract stablecoin backers pegged to the quote's fiat
//     (crypto:USDT / crypto:USDC / … — sorted), so CEX USD depth is
//     found when no classic peg traded (and EUR-quoted charts reach
//     crypto:EURC etc. via aggregate.FiatBackers).
//
// Each proxy quote is crossed with both base aliases (native ↔
// crypto:XLM, per assetAliases). The literal pair the caller already
// tried is skipped; duplicates are dropped, first occurrence kept; and a
// combination whose two sides are one asset in two spellings (sameAsset)
// is dropped rather than read, since it can never be a market.
func (s *Server) chartFiatProxyPairs(pair canonical.Pair) []canonical.Pair {
	var quotes []canonical.Asset
	// (1) operator classic pegs — USD only (they carry issuer identity
	// and are mapped to fiat only for USD by the operator's allow-list).
	if pair.Quote.Code == "USD" {
		quotes = append(quotes, s.usdPegProxyQuotes()...)
	}
	// (2) abstract stablecoin backers for the quote's fiat, sorted.
	backers := aggregate.FiatBackers(pair.Quote.Code)
	sort.Strings(backers)
	for _, code := range backers {
		if a, err := canonical.NewCryptoAsset(code); err == nil {
			quotes = append(quotes, a)
		}
	}

	literal := pair.Base.String() + "\x00" + pair.Quote.String()
	seen := make(map[string]struct{}, len(quotes)*2)
	out := make([]canonical.Pair, 0, len(quotes)*2)
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
				continue // caller already tried the literal pair
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

// chartPointsWithAliases runs `read` against each XLM dual-form alias pair
// and returns the FIRST non-empty series — the chart-side twin of
// [Server.historyPointsWithAliases] / [Server.ohlcSeriesWithAliases].
//
// The literal-keyed read alone left every chart surface blind to the venues
// publishing XLM under the other id: `?asset=native` read only
// native/<quote> buckets while the CEX-fed series lives under
// `crypto:XLM/<quote>`. [Server.chartStablecoinFallback] did not cover the
// gap — it crosses the base aliases with PROXY quotes only and skips the
// requested quote, so the one pair holding the answer
// (`crypto:XLM/fiat:USD`) was the one combination never read, and a chart
// that did fall through to a peg was needlessly stamped triangulated.
//
// An alias form is the same asset in another canonical spelling, not a
// proxy, so a hit here does NOT raise flags.triangulated — the same
// distinction [Server.fiatCombinedTrades] draws. First-hit rather than a
// cross-form combine: blending buckets across alias forms would publish a
// VWAP no venue set produced, exactly the gate the /v1/vwap point path
// applies. The literal form is tried first, so a populated pair costs one
// read; the caller's deadline covers all of them.
func (s *Server) chartPointsWithAliases(
	ctx context.Context,
	pair canonical.Pair,
	read func(context.Context, canonical.Pair) ([]HistoryPoint, error),
) ([]HistoryPoint, error) {
	for _, b := range assetAliases(pair.Base) {
		for _, q := range assetAliases(pair.Quote) {
			ap, perr := canonical.NewPair(b, q)
			if perr != nil {
				continue // degenerate alias combination (identity pair)
			}
			points, err := read(ctx, ap)
			if err != nil || len(points) > 0 {
				return points, err
			}
		}
	}
	return nil, nil
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
// Both legs are read through [Server.chartPointsWithAliases], so the
// asset leg reaches the SAC-quoted Soroban pools (asset-SAC/XLM-SAC)
// and the XLM leg reaches the CEX series stored under `crypto:XLM`;
// each leg takes its first populated spelling, the same first-hit
// contract as every other series read. The asset leg is read first so
// an asset with no XLM market at all — the common miss — costs no XLM
// read. Only buckets present on BOTH legs are emitted; a leg the reader
// truncated at its row cap yields the overlap, never a mismatched
// product. Base-side buckets carry the asset's own USD volume, which is
// what the derived series reports. An XLM base (any spelling) and a fiat
// base are not crossed: the former is the anchor itself and was already
// read literally, the latter is fx_quotes' surface.
func (s *Server) fiatSeriesThroughXLM(
	ctx context.Context, pair canonical.Pair,
	read func(context.Context, canonical.Pair) ([]HistoryPoint, error),
) ([]HistoryPoint, bool) {
	legs, ok := fiatCrossLegsThroughXLM(pair)
	if !ok {
		return nil, false
	}
	assetPts, err := s.chartPointsWithAliases(ctx, legs[0], read)
	if err != nil || len(assetPts) == 0 {
		return nil, false
	}
	xlmPts, err := s.chartPointsWithAliases(ctx, legs[1], read)
	if err != nil || len(xlmPts) == 0 {
		return nil, false
	}
	crossed := crossSeriesThroughPivot(assetPts, xlmPts)
	return crossed, len(crossed) > 0
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
		writeJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{})
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
		writeJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{})
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
	writeJSON(w, series, Flags{})
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
	writeJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{Stale: true})
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
		writeJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// USD price series (daily), with the stablecoin-USD proxy fallback
	// the normal chart uses when nothing trades directly in fiat:USD.
	pricePts, err := s.chartPointsWithAliases(ctx, pair, s.chartVWAPReader(gran, from))
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
		writeJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{Stale: true})
		return
	}
	triangulated := false
	if len(pricePts) == 0 {
		if fp, ok := s.chartStablecoinFallback(ctx, pair, s.chartVWAPReader(gran, from)); ok {
			pricePts = fp
			triangulated = true
		}
	}

	// dex-nonstandard-decimals forward normalization on the USD price
	// leg — see handleChart's equivalent comment. baseDec ALSO scales the
	// supply leg below (M2): circulating supply is denominated in the base
	// token's own smallest unit, so a confirmed non-7-decimals token's supply
	// must be divided by 10^baseDec — not the old hardcoded 10^7 — or the cap
	// is off by 10^(baseDec−7). Both legs share the SAME baseDec so
	// market_cap = supply × price stays internally coherent.
	baseDec := aggregate.ResolveDecimals(s.nonstandardDecimals, pair.Base)
	quoteDec := aggregate.ResolveDecimals(s.nonstandardDecimals, pair.Quote)
	pricePts = adjustHistoryPointPrices(pricePts, baseDec, quoteDec)

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
		writeJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{Stale: true})
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
	writeJSON(w, series, Flags{Triangulated: triangulated})
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
