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

// ohlcDefaultOutlierSigma matches the aggregator default: High/Low are not robust, so one 1-stroop SDEX dust trade at price=1 would peg a
// bar's High.
// Override via ?outlier_sigma=N (0 = raw).
const ohlcDefaultOutlierSigma = 4.0

// OHLCBar is the /v1/ohlc entry with decimal-string prices; volumes are RAW smallest-unit sums in a per-SOURCE scale (stated by BaseVolumeDecimals/QuoteVolumeDecimals), and Truncated means Open/High/Low cover only the last N trades.
type OHLCBar struct {
	From        WireTime `json:"from"`
	To          WireTime `json:"to"`
	Open        string   `json:"open"`
	High        string   `json:"high"`
	Low         string   `json:"low"`
	Close       string   `json:"close"`
	BaseVolume  string   `json:"base_volume"`
	QuoteVolume string   `json:"quote_volume"`
	// BaseVolumeDecimals / QuoteVolumeDecimals are the scale of the sums above. `null` when a
	// contributing source has no [external.Registry] entry: the sums are then not convertible
	// and must not be divided by a guessed scale.
	BaseVolumeDecimals  *int `json:"base_volume_decimals"`
	QuoteVolumeDecimals *int `json:"quote_volume_decimals"`
	TradeCount          int  `json:"trade_count"`
	Truncated           bool `json:"truncated"`
	// Clamped is true when the requested `to` was inside the
	// still-filling bucket (or in the future) and was pulled back to
	// the last closed boundary per ADR-0015.
	Clamped bool `json:"clamped"`
}

// ohlcPriceDigits is the fractional digits on wire OHLC prices, matching /v1/history; a floor, not a cap (see [priceRenderScale]).
const ohlcPriceDigits = 10

// priceRenderSigDigits is how many significant digits [ratToDecimal] keeps when a price is too small
// for its scale; priceRenderMaxScale bounds the extension. Both mirror the aggregator's
// formatRatFixed so the served and stored strings agree digit for digit.
const (
	priceRenderSigDigits = 12
	priceRenderMaxScale  = 60
)

// priceRenderScale returns the fractional places a wire price needs: extended only when a fixed `digits` render would reparse a positive
// price as 0.
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

// handleOHLC serves GET /v1/ohlc in two modes: single-bar (no `interval`: one [OHLCBar] for [from, to) via
// [aggregate.ComputeOHLC]) and series (`interval` 1m..1w: up to `limit` closed bars from prices_<n>, default 100, max 1000).
// Single-bar defaults match /v1/history.
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
		// Series mode reads the prices_<n> CAGGs, a raw quote/base ratio for a non-7-decimals leg;
		// handleOHLCSeries normalizes it via aggregate.AdjustPrice rather than declining.
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

	// The single-bar path scans raw `trades` per query, capped at maxTradesForOHLC (a larger window
	// yields a partial bar with Truncated set). The series path reads CAGGs with its own timeout.
	// 8s matches the sibling raw-scan endpoints and fires before the blanket request timeout.
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

	// The volume sums are only meaningful with a per-source scale attached. It is resolved over the
	// window as fetched: an unregistered source the filter drops still set the scale its survivors
	// were lifted to.
	volumeDecimals := commonAmountScaleDecimals(window)

	// Capture the pre-filter length so Truncated reflects whether the window hit the cap; computed
	// after FilterOutliers it would be a false negative whenever the filter dropped a trade.
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

// filterOHLCOutliers applies the single-bar outlier filter; a window with trades but none kept is a 422 (the filter withheld them), not
// "no trades".
// ok=false means the response is already written.
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

// computeOHLCSingleBar derives the bar and applies dex-nonstandard-decimals normalization; O/H/L/C share one raw ratio, so one
// per-pair scalar is exact ([aggregate.AdjustPrice]). ok=false means the failure response is already written.
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

// commonAmountScaleDecimals is the scale of a window's volume sums: the MAXIMUM per-source scale ([aggregate.NormalizeAmountScale]), so
// lifts are exact (ADR-0003).
// Call it over the unfiltered window; zero for an empty window (404). [barScaleDecimals] is the series twin via [amountScaleDecimalsFor].
// Returns [ohlcBarScaleUnknown] if a source is missing from [external.Registry]: [external.Lookup] would default to CEX 8 and misstate a
// 7dp DEX tenfold.
func commonAmountScaleDecimals(window aggregate.ScaledWindow) int {
	trades := window.Trades()
	for i := range trades {
		if !external.Registered(trades[i].Source) {
			return ohlcBarScaleUnknown
		}
	}
	return window.Decimals()
}

// parseOHLCOutlierSigma parses ?outlier_sigma=N, defaulting to [ohlcDefaultOutlierSigma] (non-zero, unlike /v1/vwap: High/Low need the
// floor).
// outlier_sigma=0 opts out for raw extremes; ok=false means a problem+json was written.
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

// parseFromTo parses from/to with a 1-hour default; `to` defaults to wall-clock now (exact range, /v1/history; rate endpoints use
// [parseFromToClamped], ADR-0015).
// `window` (a [time.ParseDuration] string plus `d`) sets from = to - window; with an explicit `from` it is a 400, never a silent 1h
// default.
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

// parseWindowDuration accepts [time.ParseDuration] units plus a trailing-`d` days shortcut (`7d`).
// Mixed units with `d` (`1d12h`) are rejected as ambiguous; clients can express them in hours.
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

// closedBucketWindow is the boundary [parseFromToClamped] snaps "now" to: inside the smallest CAGG bucket (prices_1m), matching the 30 s
// freshness SLA.
// Per ADR-0015, regions clamp to the same boundary and answer over the identical window, so JSON is byte-identical.
const closedBucketWindow = 30 * time.Second

// parseFromToClamped is the rate-endpoint flavour of [parseFromTo]: the right edge is min(to, now.Truncate([closedBucketWindow]))
// (ADR-0015);
// a `to` already before the boundary is left verbatim and not reported as clamped.
func parseFromToClamped(w http.ResponseWriter, r *http.Request) (from, to time.Time, clamped, ok bool) {
	toExplicit := r.URL.Query().Get("to") != ""
	from, to, ok = parseFromTo(w, r)
	if !ok {
		return time.Time{}, time.Time{}, false, false
	}
	// ADR-0015: the right edge of the DATA is always closed, so an explicit `to` inside the
	// still-filling bucket must clamp too, or asking explicitly would escape the contract. `nowRef`
	// is `to` itself when defaulted; an explicit `to` is compared against a fresh wall-clock now.
	nowRef := to
	if toExplicit {
		nowRef = time.Now().UTC()
	}
	closedTo := nowRef.Truncate(closedBucketWindow)
	if to.After(closedTo) {
		// A defaulted `from` shifts by the same delta so the window stays 1h; an explicit one is
		// kept, preserving the client's range up to the new closed edge.
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

// ratToDecimal renders a *big.Rat as a decimal string, truncating (flooring) with at least `digits` places, more when a tiny price needs
// it ([priceRenderScale]).
// Returns "0" for nil.
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
