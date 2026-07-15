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
	points, err := s.history.HistoryPointsInRange(chartCtx, pair, gran, from, time.Time{}, historyMaxPoints)
	if errors.Is(err, ErrUnknownGranularity) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-granularity",
			"Invalid granularity", http.StatusBadRequest,
			fmt.Sprintf("granularity must be one of: 1m, 15m, 1h, 4h, 1d, 1w, 1mo (got %q)", gran))
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

	writeJSON(w, series, Flags{Triangulated: triangulated})
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

	points, err := read(ctx, pair)
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
// fx_quotes hypertable. Frankfurter (and historically Massive) writes
// daily ECB reference rates into fx_quotes — so any sub-daily
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
		s.logger.Warn("chart fiat fx_quotes fetch failed",
			"ticker", ticker, "err", err)
		writeJSON(w, series, Flags{})
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
		s.logger.Warn("chart fiat-cross fx_quotes fetch failed",
			"ticker", pair.Base.Code, "err", err)
		writeJSON(w, series, Flags{})
		return
	}
	quotePts, err := s.fxHistory.ListFXHistory(fxCtx, pair.Quote.Code, queryFrom, to)
	if err != nil {
		s.logger.Warn("chart fiat-cross fx_quotes fetch failed",
			"ticker", pair.Quote.Code, "err", err)
		writeJSON(w, series, Flags{})
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
// ok=false when no fallback fires (caller keeps the empty result +
// leaves triangulated=false).
//
// `read` fetches one proxied pair's closed-bucket series — the VWAP
// path passes a prices_<gran> reader, the TWAP path a twap_<gran>
// reader — so both CAGG-reading chart surfaces share the same
// stablecoin-proxy fallback.
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
	return nil, false
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
//  1. operator USD-pegged classics (config order) — keeps classic
//     Circle USDC winning where it has data;
//  2. abstract stablecoin backers pegged to the quote's fiat
//     (crypto:USDT / crypto:USDC / … — sorted), so CEX USD depth is
//     found when no classic peg traded (and EUR-quoted charts reach
//     crypto:EURC etc. via aggregate.FiatBackers).
//
// Each proxy quote is crossed with both base aliases (native ↔
// crypto:XLM, per assetAliases). The literal pair the caller already
// tried is skipped; duplicates are dropped, first occurrence kept.
func (s *Server) chartFiatProxyPairs(pair canonical.Pair) []canonical.Pair {
	var quotes []canonical.Asset
	// (1) operator classic pegs — USD only (they carry issuer identity
	// and are mapped to fiat only for USD by the operator's allow-list).
	if pair.Quote.Code == "USD" {
		quotes = append(quotes, s.usdPeggedClassics...)
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
			if q.Equal(b) {
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

// chartVWAPReader returns a [chartStablecoinFallback] read closure that
// fetches a pair's closed prices_<gran> series over [from, now).
func (s *Server) chartVWAPReader(gran string, from time.Time) func(context.Context, canonical.Pair) ([]HistoryPoint, error) {
	return func(ctx context.Context, p canonical.Pair) ([]HistoryPoint, error) {
		return s.history.HistoryPointsInRange(ctx, p, gran, from, time.Time{}, historyMaxPoints)
	}
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
	m2, err := parseSupply(vc.CirculatingSupply, vc.SupplyDecimals)
	if err != nil {
		s.logger.Warn("market_cap: bad catalogue supply",
			"ticker", vc.Ticker, "err", err)
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
		s.logger.Warn("market_cap: fx_quotes fetch failed",
			"ticker", pair.Base.Code, "err", err)
		writeJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{})
		return
	}

	wire := make([]HistoryPointWire, 0, len(points))
	for _, p := range points {
		if p.InverseUSD <= 0 {
			continue
		}
		mcap := m2 * p.InverseUSD
		wire = append(wire, HistoryPointWire{
			T: p.Bucket,
			P: fmt.Sprintf("%.2f", mcap),
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

// emptyMarketCapSeries is the no-data response shape used when the
// catalogue doesn't carry a supply for the asset or the FX feed has
// no rows for the requested window. Keeping it as a helper means
// every error path emits the same wire shape (empty points array,
// not null).
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

// marketCapDecimals is the per-unit scale handleChartMarketCapCrypto
// divides supply by. 7 is correct for native XLM + every classic
// asset; SEP-41 Soroban tokens can declare other decimals, but 7 is
// the Stellar/SAC default and matches the spot market_cap path
// (populateMarketCap → detail.Decimals defaults to 7), so the chart
// and the headline cap stay consistent. Refining per-token decimals
// is the same follow-up the spot path carries.
const marketCapDecimals = 7

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
	pricePts, err := s.history.HistoryPointsInRange(ctx, pair, gran, from, time.Time{}, historyMaxPoints)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Warn("market_cap crypto: price history failed",
			"asset", pair.Base.String(), "err", err)
		writeJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{})
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
	// leg — see handleChart's equivalent comment. (The supply leg's own
	// decimals scaling — marketCapDecimals — is a separate, already-
	// acknowledged follow-up; not touched here.)
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
		s.logger.Warn("market_cap crypto: supply history failed",
			"asset_key", supplyKey, "err", err)
		writeJSON(w, emptyMarketCapSeries(pair, tfRaw, gran, from), Flags{})
		return
	}

	wire := marketCapPoints(pricePts, supPts)
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
func marketCapPoints(pricePts []HistoryPoint, supPts []timescale.SupplyDayPoint) []HistoryPointWire {
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
		mc, err := usdMarketValue(cur, pp.VWAP, marketCapDecimals)
		if err != nil {
			continue
		}
		wire = append(wire, HistoryPointWire{T: pp.Bucket, P: mc})
	}
	return wire
}

// parseSupply converts the catalogue's (supply, decimals) tuple into
// a float64. The catalogue stores supplies as decimal strings in the
// asset's smallest integer unit (per the seed.yaml convention),
// alongside a decimals exponent. For fiat M2 the decimals are 0 so
// the supply is already in major units (e.g. "21700000000000" =
// $21.7T). For tokens decimals would be 7 / 18 / etc; we divide.
func parseSupply(supplyStr string, decimals int) (float64, error) {
	v, err := strconv.ParseFloat(supplyStr, 64)
	if err != nil {
		return 0, fmt.Errorf("parse supply %q: %w", supplyStr, err)
	}
	for i := 0; i < decimals; i++ {
		v /= 10
	}
	return v, nil
}
