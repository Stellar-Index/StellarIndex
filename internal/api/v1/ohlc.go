package v1

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// ohlcDefaultOutlierSigma is the default σ threshold for the outlier
// filter applied before ComputeOHLC. Unlike VWAP — which is volume-
// weighted and naturally dampens dust trades — OHLC's High/Low have
// no statistical robustness: a single 1-stroop ↔ 1-stroop SDEX dust
// trade lands at price=1 and pegs the High of an entire bar.
//
// 4.0 matches the aggregator orchestrator's default
// (cfg.OutlierSigmaThreshold). Caller can override via
// ?outlier_sigma=N, including 0 to disable for raw inspection.
const ohlcDefaultOutlierSigma = 4.0

// OHLCBar is the wire shape for /v1/ohlc entries. All prices are
// decimal strings (ADR-0003).
//
// The volume fields are RAW smallest-unit sums, and the smallest unit is a
// per-SOURCE scale, NOT a fixed stroop: on-chain DEX legs are 7-decimal, CEX 8
// (external.externalAmountDecimals), FX 6, as resolved by
// [amountScaleDecimalsFor]. A consumer dividing by a fixed 1e7 overstates a
// CEX-fed pair tenfold.
//
// BaseVolumeDecimals / QuoteVolumeDecimals state that scale on the wire.
// Every point window is lifted to ONE common scale by
// [aggregate.NormalizeAmountScale] before summing; the stated value is that
// lift target, resolved by [commonAmountScaleDecimals] over the
// PRE-outlier-filter population so it stays the served integers' true scale
// even when the filter removes the only max-scale venue.
//
// This is the per-SOURCE axis only: a leg in `nonstandard_decimals_assets`
// is stamped at the ASSET's own decimals, which NormalizeAmountScale does not
// model (the gap [aggregate.AdjustPrice] patches on the price axis).
//
// Truncated signals the window hit the per-request trade cap: Open/High/Low
// reflect only the chronologically-LAST N trades (the reader drops the OLDEST
// under the LIMIT). Close is unaffected. See VWAPResult.Truncated.
type OHLCBar struct {
	From        WireTime `json:"from"`
	To          WireTime `json:"to"`
	Open        string   `json:"open"`
	High        string   `json:"high"`
	Low         string   `json:"low"`
	Close       string   `json:"close"`
	BaseVolume  string   `json:"base_volume"`
	QuoteVolume string   `json:"quote_volume"`
	// BaseVolumeDecimals / QuoteVolumeDecimals are the smallest-unit
	// scale of the two sums above — see this type's doc comment. `null`
	// when a contributing trade's source has no [external.Registry]
	// entry: the sums are not convertible to asset units and must not
	// be divided by a guessed scale. Mirrors
	// OHLCSeriesBar.v_base_decimals / v_quote_decimals.
	BaseVolumeDecimals  *int `json:"base_volume_decimals"`
	QuoteVolumeDecimals *int `json:"quote_volume_decimals"`
	TradeCount          int  `json:"trade_count"`
	Truncated           bool `json:"truncated"`
	// Clamped is true when the requested `to` was inside the
	// still-filling bucket (or in the future) and was pulled back to
	// the last closed boundary per ADR-0015.
	Clamped bool `json:"clamped"`
}

// ohlcPriceDigits is how many fractional digits the wire OHLC
// prices carry. Ten is generous enough to represent sub-stroop
// prices without being absurd — consistent with the /v1/history
// price field. It is a floor, not a cap: see [priceRenderScale].
const ohlcPriceDigits = 10

// priceRenderSigDigits is how many significant digits [ratToDecimal]
// keeps when a price is too small for its requested scale;
// priceRenderMaxScale bounds the extension. Both mirror the
// aggregator's formatRatFixed so the served string and the stored one
// agree digit for digit.
const (
	priceRenderSigDigits = 12
	priceRenderMaxScale  = 60
)

// priceRenderScale returns the fractional places a wire price needs.
// The fixed `digits` render of a non-zero rational is all zeros — and
// reparses as price 0 — exactly when its first significant digit lies
// beyond the last rendered place; only then is the scale extended,
// magnitude-relatively, so no positive price ever serves as zero.
// Every other value keeps the requested scale, byte-identical.
func priceRenderScale(r *big.Rat, digits int) int {
	if r == nil || r.Sign() == 0 {
		return digits
	}
	x := new(big.Rat).Abs(r)
	one := big.NewRat(1, 1)
	ten := big.NewRat(10, 1)
	firstSigPlace := 0
	for x.Cmp(one) < 0 && firstSigPlace < priceRenderMaxScale {
		x.Mul(x, ten)
		firstSigPlace++
	}
	if firstSigPlace <= digits {
		return digits
	}
	if need := firstSigPlace + priceRenderSigDigits; need < priceRenderMaxScale {
		return need
	}
	return priceRenderMaxScale
}

// handleOHLC serves GET /v1/ohlc?base=...&quote=...&from=...&to=...
//
// Two modes share this route:
//
//  1. Single-bar (default): no `interval` query param. Returns one
//     [OHLCBar] for the window [from, to) computed from raw trades
//     via [aggregate.ComputeOHLC].
//  2. Multi-bar series (CG/CMC parity): `interval` is one
//     of 1m / 5m / 15m / 30m / 1h / 4h / 1d / 1w. Returns
//     [OHLCSeriesResponse.Intervals] — up to `limit` (default 100,
//     max 1000) closed bars, oldest first, sourced from the
//     prices_<n> continuous aggregates.
//
// Defaults (single-bar mode) match /v1/history:
//   - from: to - 1h
//   - to:   now (clamped to the previous closed-bucket boundary)
//
// Defaults (series mode) are interval-aware:
//   - to:   now snapped DOWN to interval boundary
//   - from: to - limit*interval
func (s *Server) handleOHLC(w http.ResponseWriter, r *http.Request) {
	reader := s.History
	if reader == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/ohlc-unavailable",
			"OHLC serving not configured", http.StatusServiceUnavailable,
			"this deployment has no HistoryReader wired — check binary configuration")
		return
	}

	base, quote, ok := parseBaseQuote(w, r)
	if !ok {
		return
	}
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-pair",
			"Invalid pair", http.StatusBadRequest, err.Error())
		return
	}

	// Branch to the multi-bar series handler when `interval` is
	// supplied. Invalid intervals 400 before any other work.
	if raw := r.URL.Query().Get("interval"); raw != "" {
		interval, ok := parseOHLCInterval(w, r, raw)
		if !ok {
			return
		}
		// dex-nonstandard-decimals: series mode reads the prices_<n>
		// continuous aggregates (migration 0002), a raw (unnormalized)
		// quote/base ratio for a confirmed non-7-decimals leg — same as
		// the single-bar branch below, normalized via aggregate.AdjustPrice
		// inside handleOHLCSeries rather than declined. See
		// docs/operations/runbooks/dex.md "Root
		// cause analysis".
		s.handleOHLCSeries(w, r, pair, interval)
		return
	}

	// Clamped to a closed-bucket boundary per ADR-0015, whether `to`
	// was defaulted or explicit.
	from, to, clamped, ok := parseFromToClamped(w, r)
	if !ok {
		return
	}

	sigma, ok := parseOHLCOutlierSigma(w, r)
	if !ok {
		return
	}

	// The single-bar path scans raw `trades` on every query, capped at
	// maxTradesForOHLC; a window with more trades than that yields a
	// partial bar with Truncated set. The multi-bar series path above
	// reads CAGGs and sets its own timeout in handleOHLCSeries.
	// 8s matches the sibling raw-scan endpoints and fires before the
	// blanket request-timeout middleware.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// Single-bar mode shares the point-path trade fetch with /v1/vwap and
	// /v1/twap — one implementation, so a fiat quote can't be resolved
	// against a different constituent set here than there.
	const maxTradesForOHLC = 10000
	window, triangulated, err := s.tradesInRangeWithStablecoinFallback(ctx, pair, from, to, maxTradesForOHLC)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("TradesInRange failed for OHLC",
			"err", err,
			"base", base.String(), "quote", quote.String(),
			"from", from, "to", to)
		writeProblemErr(w, r, err,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	// The volume sums below are only meaningful with a scale attached, and
	// the scale is per-SOURCE (7dp on-chain, 8 CEX, 6 FX). It is
	// resolved over the window as fetched: the window carries its lift
	// target through the outlier filter, and an unregistered source the
	// filter drops still set the scale its survivors were lifted to.
	volumeDecimals := commonAmountScaleDecimals(window)

	// Capture the pre-filter length so Truncated reflects whether the
	// WINDOW hit the cap — not whether the post-outlier-filter slice
	// happens to equal it. Mirrors vwap.go; computing it after
	// FilterOutliers would yield false negatives whenever the filter
	// dropped any trade.
	preFilter := window.Len()
	if window, ok = filterOHLCOutliers(w, r, window, sigma); !ok {
		return
	}

	bar, ok := s.computeOHLCSingleBar(w, r, pair, from, to, window.Trades())
	if !ok {
		return
	}

	writeJSON(w, OHLCBar{
		From:                WireTime(from),
		To:                  WireTime(to),
		Open:                ratToDecimal(bar.Open, ohlcPriceDigits),
		High:                ratToDecimal(bar.High, ohlcPriceDigits),
		Low:                 ratToDecimal(bar.Low, ohlcPriceDigits),
		Close:               ratToDecimal(bar.Close, ohlcPriceDigits),
		BaseVolume:          bar.BaseVolume.String(),
		QuoteVolume:         bar.QuoteVolume.String(),
		BaseVolumeDecimals:  wireScaleDecimals(volumeDecimals),
		QuoteVolumeDecimals: wireScaleDecimals(volumeDecimals),
		TradeCount:          bar.TradeCount,
		Truncated:           preFilter == maxTradesForOHLC,
		Clamped:             clamped,
	}, Flags{Triangulated: triangulated, ProxyDeviation: triangulated && s.proxyDeviation(ctx, to)})
}

// filterOHLCOutliers applies the single-bar outlier filter. A window
// that had trades but kept none is a 422, not "no trades": the filter
// withheld them — including a contested window whose trim would have
// discarded most of its base volume. Returns ok=false when it has
// already written the response.
func filterOHLCOutliers(w http.ResponseWriter, r *http.Request, window aggregate.ScaledWindow, sigma float64) (aggregate.ScaledWindow, bool) {
	if sigma <= 0 {
		return window, true
	}
	pre := window.Len()
	window = window.FilterOutliers(sigma)
	if pre > 0 && window.Len() == 0 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/all-filtered",
			"All trades filtered as outliers", http.StatusUnprocessableEntity,
			fmt.Sprintf("outlier_sigma=%v removed all %d trades in window; relax the threshold or pass outlier_sigma=0 for the unfiltered bar",
				sigma, pre))
		return aggregate.ScaledWindow{}, false
	}
	return window, true
}

// computeOHLCSingleBar folds the single-bar path's compute-and-normalize
// tail into one seam: it derives the bar, answers the caller directly on
// the two failure shapes, and applies the dex-nonstandard-decimals
// forward normalization before handing the bar back.
//
// Normalization lives here rather than at the call site because
// ComputeOHLC derives every one of Open/High/Low/Close from the same raw
// quote/base ratio VWAP uses, so the SAME per-pair scalar factor corrects
// all four — see [aggregate.AdjustPrice] for why a post-hoc multiply is
// exact. No-op for a pair with no confirmed non-7-decimals leg.
//
// Returns ok=false when it has already written the response.
func (s *Server) computeOHLCSingleBar(
	w http.ResponseWriter, r *http.Request,
	pair canonical.Pair, from, to time.Time,
	trades []canonical.Trade,
) (*aggregate.OHLC, bool) {
	bar, err := aggregate.ComputeOHLC(trades)
	if errors.Is(err, aggregate.ErrNoTrades) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/no-trades",
			"No trades in window", http.StatusNotFound,
			"no trades observed for "+pair.Base.String()+"/"+pair.Quote.String()+
				" between "+from.Format(time.RFC3339)+" and "+to.Format(time.RFC3339))
		return nil, false
	}
	if err != nil {
		s.logger.Error("ComputeOHLC failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return nil, false
	}

	baseDec := aggregate.ResolveDecimals(s.NonstandardDecimals, pair.Base)
	quoteDec := aggregate.ResolveDecimals(s.NonstandardDecimals, pair.Quote)
	bar.Open = aggregate.AdjustPrice(bar.Open, baseDec, quoteDec)
	bar.High = aggregate.AdjustPrice(bar.High, baseDec, quoteDec)
	bar.Low = aggregate.AdjustPrice(bar.Low, baseDec, quoteDec)
	bar.Close = aggregate.AdjustPrice(bar.Close, baseDec, quoteDec)
	return bar, true
}

// commonAmountScaleDecimals is the smallest-unit scale a window's volume
// sums are in: the lift target [aggregate.NormalizeAmountScale] chose, the
// MAXIMUM per-source scale present (max, so every lift is an exact integer
// multiply and nothing is divided — ADR-0003).
//
// Call it over the window as fetched, not after the outlier filter: the
// registry check below must see every source the lift was resolved over.
//
// Zero for an empty window, which never reaches the wire — ComputeOHLC
// 404s on ErrNoTrades first.
//
// [barScaleDecimals] is the series arm's bar-level twin, over a CAGG row's
// `sources` column; both resolve a venue through [amountScaleDecimalsFor]
// so the point and series paths cannot disagree about a source's scale.
// Returns [ohlcBarScaleUnknown] if any contributing trade's source has
// no [external.Registry] entry: [external.Lookup] answers such a source
// with the registry's CEX-flavoured 8-decimal default (AmountScaleDecimals'
// zero-value fallback), and stating that default as fact for a source we
// do not actually recognise would misstate the scale tenfold for
// the opposite population — an unregistered on-chain DEX at 7 decimals
// would be reported at 8. An unrecognised source is scale-unknown, not
// scale-8.
func commonAmountScaleDecimals(window aggregate.ScaledWindow) int {
	trades := window.Trades()
	for i := range trades {
		if !external.Registered(trades[i].Source) {
			return ohlcBarScaleUnknown
		}
	}
	return window.Decimals()
}

// parseOHLCOutlierSigma parses the optional ?outlier_sigma=N query
// parameter, defaulting to [ohlcDefaultOutlierSigma]. Mirrors
// /v1/vwap's parser but with a non-zero default — see the constant
// docs for why OHLC needs the floor.
//
// `outlier_sigma=0` is the explicit opt-out: callers who want raw
// per-trade extremes (e.g. the explorer's "show every print" view)
// pass it to disable filtering entirely.
//
// Reports ok=false after writing a problem+json on parse failure.
func parseOHLCOutlierSigma(w http.ResponseWriter, r *http.Request) (float64, bool) {
	raw := r.URL.Query().Get("outlier_sigma")
	if raw == "" {
		return ohlcDefaultOutlierSigma, true
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-sigma",
			"Invalid outlier_sigma", http.StatusBadRequest,
			"outlier_sigma must be a non-negative finite number; omit for the default ("+strconv.FormatFloat(ohlcDefaultOutlierSigma, 'f', -1, 64)+") or 0 to disable filtering")
		return 0, false
	}
	return v, true
}

// parseFromTo parses the from/to query params, applying the same
// 1-hour default as /v1/history. Writes problem + returns ok=false
// on failure.
//
// `to` defaults to the request's wall-clock now. Use this for
// /v1/history where the client wants exactly the trades in the
// stated range — the API mustn't quietly snap their range to a
// boundary. For aggregated rate endpoints (VWAP/TWAP/OHLC) use
// [parseFromToClamped] instead, per ADR-0015.
//
// `window` is an optional convenience for CG-style customers who
// don't want to compute `from = now - duration` themselves. Accepted
// formats follow [time.ParseDuration] (ns/us/ms/s/m/h) plus a
// trailing-`d` shortcut for days (e.g. `7d`). When supplied, `from`
// is set to `to - window`. Combining `window` with an explicit
// `from` is a 400 — they're conflicting controls for the same value;
// rejecting it loudly catches the "I asked for 24h and got a
// 1h default" surprise. Combining `window` with an explicit `to` is
// fine — gives an arbitrary-anchored window of the requested length.
func parseFromTo(w http.ResponseWriter, r *http.Request) (from, to time.Time, ok bool) {
	to = time.Now().UTC()
	if raw := r.URL.Query().Get("to"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-time",
				"Invalid `to` timestamp", http.StatusBadRequest,
				"to must be RFC 3339")
			return time.Time{}, time.Time{}, false
		}
		to = parsed.UTC()
	}
	from = to.Add(-time.Hour)
	windowRaw := r.URL.Query().Get("window")
	fromRaw := r.URL.Query().Get("from")
	if windowRaw != "" {
		if fromRaw != "" {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-time",
				"`window` and `from` are mutually exclusive", http.StatusBadRequest,
				"pass one or the other — `window=24h` is shorthand for `from=to-24h`")
			return time.Time{}, time.Time{}, false
		}
		d, err := parseWindowDuration(windowRaw)
		if err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-time",
				"Invalid `window` duration", http.StatusBadRequest,
				err.Error())
			return time.Time{}, time.Time{}, false
		}
		if d <= 0 {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-time",
				"`window` must be positive", http.StatusBadRequest,
				"got "+windowRaw)
			return time.Time{}, time.Time{}, false
		}
		from = to.Add(-d)
	} else if fromRaw != "" {
		parsed, err := time.Parse(time.RFC3339, fromRaw)
		if err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-time",
				"Invalid `from` timestamp", http.StatusBadRequest,
				"from must be RFC 3339")
			return time.Time{}, time.Time{}, false
		}
		from = parsed.UTC()
	}
	if !from.Before(to) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-time",
			"`from` must be before `to`", http.StatusBadRequest, "")
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// parseWindowDuration accepts the same units as [time.ParseDuration]
// (ns/us/ms/s/m/h) and additionally a trailing-`d` shortcut for days
// (e.g. `7d` = 168h). Multiple units in one string are NOT supported
// for the `d` shortcut (`1d12h` is rejected) — that ambiguity is
// best avoided in user-facing query params; clients wanting odd
// durations can express them in hours.
func parseWindowDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if last := s[len(s)-1]; last == 'd' || last == 'D' {
		days, err := strconv.Atoi(s[:len(s)-1])
		if err != nil {
			return 0, fmt.Errorf("invalid day count %q: %w", s, err)
		}
		const maxDays = int(math.MaxInt64 / int64(24*time.Hour))
		if days > maxDays || days < -maxDays {
			return 0, fmt.Errorf("day count %q out of range", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// closedBucketWindow is the boundary granularity used by
// [parseFromToClamped] when `to` defaults to "now". 30 s sits inside the
// smallest bucket of the aggregator's CAGG ladder (`prices_1m`) and
// matches the Freighter ≤30 s freshness SLA.
//
// Per ADR-0015, snapping the implicit "now" to this boundary is what
// makes "every region returns the same rate" a real property: a
// request landing at 12:00:01.234 in two regions clamps to
// 12:00:00.000 in both and answers over the identical
// [from, 12:00:00.000) window — same trades, same result, same JSON
// bytes once both regions have ingested those trades.
const closedBucketWindow = 30 * time.Second

// parseFromToClamped is the rate-endpoint flavour of [parseFromTo]:
// the effective right edge is always min(to, now.Truncate
// ([closedBucketWindow])), whether `to` came from the client or from
// parseFromTo's default — ADR-0015 says the served window's right
// edge is always closed, not just the defaulted one. A `to` that
// already precedes the boundary (a genuinely historical range) is
// left verbatim and is not reported as clamped.
//
// Sets the *clamped flag so callers can surface "this response
// reflects a closed-bucket window, not the range you asked for" in
// their wire output — see VWAPResult.Clamped / TWAPResult.Clamped /
// OHLCBar.Clamped.
func parseFromToClamped(w http.ResponseWriter, r *http.Request) (from, to time.Time, clamped, ok bool) {
	toExplicit := r.URL.Query().Get("to") != ""
	from, to, ok = parseFromTo(w, r)
	if !ok {
		return time.Time{}, time.Time{}, false, false
	}
	// ADR-0015: the most-recent row served for any window is always
	// closed — the rule is about the right edge of the DATA, not about
	// what a defaulted `to` means. Clamping only the implicit-`to` case
	// let `?to=<now>` (or any `to` inside the still-filling bucket) walk
	// out of the closed-bucket contract by asking explicitly instead of
	// omitting the param. `nowRef` is `to` itself in the implicit case
	// (already the instant parseFromTo defaulted to) so behaviour there
	// is unchanged; an explicit `to` is compared against a fresh reading
	// of wall-clock now.
	nowRef := to
	if toExplicit {
		nowRef = time.Now().UTC()
	}
	closedTo := nowRef.Truncate(closedBucketWindow)
	if to.After(closedTo) {
		// If `from` was also defaulted (= to - 1h), shift it by the
		// same delta so the window length stays 1h. If `from` was
		// explicit, leave it alone — the client's range is preserved
		// up to the new (closed) right edge.
		fromExplicit := r.URL.Query().Get("from") != ""
		if !toExplicit && !fromExplicit {
			from = from.Add(closedTo.Sub(to))
		}
		to = closedTo
		clamped = true
	}
	if !from.Before(to) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-time",
			"`from` must be before `to` after closed-bucket clamp",
			http.StatusBadRequest,
			"the requested range collapsed below a single closed window — widen `from` or specify an explicit `to`")
		return time.Time{}, time.Time{}, false, false
	}
	return from, to, clamped, true
}

// ratToDecimal renders a *big.Rat as a fixed-width decimal string
// with at least `digits` fractional places, truncating (floors) —
// the rounding choice shared by every price surface, /v1/price and
// /v1/history included, which render through it. A price too small
// for `digits` gets more places rather than zeros ([priceRenderScale]).
//
// Returns "0" for nil input.
func ratToDecimal(r *big.Rat, digits int) string {
	if r == nil {
		return "0"
	}
	if digits < 0 {
		digits = 0
	}
	digits = priceRenderScale(r, digits)
	num := new(big.Int).Set(r.Num())
	den := new(big.Int).Set(r.Denom())

	// Scale numerator by 10^digits, divide, format.
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	num.Mul(num, scale)
	integer, _ := new(big.Int).DivMod(num, den, new(big.Int))

	// Preserve sign explicitly — Int.DivMod handles negatives but the
	// format below is easier to reason about without it.
	sign := ""
	if integer.Sign() < 0 {
		sign = "-"
		integer.Abs(integer)
	}

	s := integer.String()
	if digits == 0 {
		return sign + s
	}
	if len(s) <= digits {
		pad := digits - len(s) + 1
		s = leftPad(s, pad, '0')
	}
	split := len(s) - digits
	return sign + s[:split] + "." + s[split:]
}
