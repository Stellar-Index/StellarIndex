package v1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// OHLCSeriesBar is one bar of the multi-bar /v1/ohlc response with short field names; `T` is the UTC-aligned bucket start (1w starts Monday 00:00 UTC).
type OHLCSeriesBar struct {
	T      WireTime `json:"t"`
	O      string   `json:"o"`
	H      string   `json:"h"`
	L      string   `json:"l"`
	C      string   `json:"c"`
	VBase  string   `json:"v_base"`
	VQuote string   `json:"v_quote"`
	// VBaseDecimals / VQuoteDecimals state the scale of VBase/VQuote: the per-source scale
	// [barScaleDecimals] resolves from the bucket's venues (7dp on-chain, 8 CEX, 6 FX), not a fixed
	// stroop, so dividing v_quote by 1e7 overstates a CEX-fed bucket tenfold. They are equal because
	// a source stamps both legs at one scale and a combined bucket's lift target
	// ([ohlcBucketAcc.commonScale]) is per bucket. Null when unknown ([ohlcBarScaleUnknown]).
	VBaseDecimals  *int  `json:"v_base_decimals"`
	VQuoteDecimals *int  `json:"v_quote_decimals"`
	N              int64 `json:"n"`
	Truncated      bool  `json:"truncated,omitempty"`

	// Sources is the set of venues that contributed to the bucket, from the CAGG's `sources`
	// column. It is the per-bar provenance (a derived or vendor bar names its source, so it never
	// reads as a fill-derived VWAP) and lets a caller combining bars across markets lift them to a
	// common scale first ([Server.ohlcSeriesFiatCombined], [annotateOHLCSeriesBarScale]).
	Sources []string `json:"sources,omitempty"`
}

// OHLCSeriesResponse is the CG-parity series envelope for /v1/ohlc?interval=..., distinct from the single-bar [OHLCBar] response.
type OHLCSeriesResponse struct {
	Base      string          `json:"base"`
	Quote     string          `json:"quote"`
	Interval  string          `json:"interval"`
	From      WireTime        `json:"from"`
	To        WireTime        `json:"to"`
	Intervals []OHLCSeriesBar `json:"intervals"`
}

// ohlcSeriesDefaultLimit / ohlcSeriesMaxLimit bound the bar count of one /v1/ohlc?interval=
// request. The max matches CoinGecko's `/ohlc` cap; larger windows are an explicit opt-in.
const (
	ohlcSeriesDefaultLimit = 100
	ohlcSeriesMaxLimit     = 1000
)

// ohlcInterval is a validated multi-bar interval; the accepted set is [timescale.OHLCRoutes], so [parseOHLCInterval] ranges over it.
// TestOHLCIntervals_EveryRouteHasADuration keeps the named constants in step.
type ohlcInterval string

const (
	ohlcInterval1m  ohlcInterval = "1m"
	ohlcInterval5m  ohlcInterval = "5m"
	ohlcInterval15m ohlcInterval = "15m"
	ohlcInterval30m ohlcInterval = "30m"
	ohlcInterval1h  ohlcInterval = "1h"
	ohlcInterval2h  ohlcInterval = "2h"
	ohlcInterval4h  ohlcInterval = "4h"
	ohlcInterval12h ohlcInterval = "12h"
	ohlcInterval1d  ohlcInterval = "1d"
	ohlcInterval3d  ohlcInterval = "3d"
	ohlcInterval1w  ohlcInterval = "1w"
	ohlcInterval2w  ohlcInterval = "2w"
	ohlcInterval1mo ohlcInterval = "1mo"
)

// duration returns the [time.Duration] of an interval, used for default-window sizing. 1w is
// 7*24h; DST is irrelevant for UTC alignment.
func (i ohlcInterval) duration() time.Duration {
	switch i {
	case ohlcInterval1m:
		return 1 * time.Minute
	case ohlcInterval5m:
		return 5 * time.Minute
	case ohlcInterval15m:
		return 15 * time.Minute
	case ohlcInterval30m:
		return 30 * time.Minute
	case ohlcInterval1h:
		return 1 * time.Hour
	case ohlcInterval2h:
		return 2 * time.Hour
	case ohlcInterval4h:
		return 4 * time.Hour
	case ohlcInterval12h:
		return 12 * time.Hour
	case ohlcInterval1d:
		return 24 * time.Hour
	case ohlcInterval3d:
		return 3 * 24 * time.Hour
	case ohlcInterval1w:
		return 7 * 24 * time.Hour
	case ohlcInterval2w:
		return 14 * 24 * time.Hour
	case ohlcInterval1mo:
		// Calendar months vary; 30d is only used for default-window
		// sizing (N × interval), where approximation is harmless —
		// the CAGG buckets themselves are true calendar months.
		return 30 * 24 * time.Hour
	}
	return 0
}

// parseOHLCInterval validates `interval` against [timescale.OHLCRoutes]; an empty raw yields ok=false with no response, so callers tell
// "no interval" from "invalid interval" via raw != "".
func parseOHLCInterval(w http.ResponseWriter, r *http.Request, raw string) (ohlcInterval, bool) {
	if _, ok := timescale.OHLCRouteFor(raw); ok {
		return ohlcInterval(raw), true
	}
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/invalid-interval",
		"Invalid interval", http.StatusBadRequest,
		"interval must be one of: "+timescale.OHLCIntervalList()+" (got "+strconv.Quote(raw)+")")
	return "", false
}

// parseOHLCSeriesLimit parses ?limit= for series mode (default [ohlcSeriesDefaultLimit], cap [ohlcSeriesMaxLimit]); ok=false after
// writing a problem+json.
func parseOHLCSeriesLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return ohlcSeriesDefaultLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > ohlcSeriesMaxLimit {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/limit-too-large",
			"Invalid limit", http.StatusBadRequest,
			fmt.Sprintf("limit must be an integer in [1, %d]", ohlcSeriesMaxLimit))
		return 0, false
	}
	return n, true
}

// handleOHLCSeries serves the multi-bar branch of /v1/ohlc, closed buckets only (ADR-0015): storage excludes the in-progress bucket and a
// defaulted `to`
// snaps to the previous interval boundary so regions answer byte-identically. [Server.handleOHLC] routes here only with a validated
// interval.
func (s *Server) handleOHLCSeries(
	w http.ResponseWriter, r *http.Request,
	pair canonical.Pair,
	interval ohlcInterval,
) {
	limit, ok := parseOHLCSeriesLimit(w, r)
	if !ok {
		return
	}

	from, to, ok := parseOHLCSeriesFromTo(w, r, interval, limit)
	if !ok {
		return
	}

	hCtx, hCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer hCancel()
	// One row past the cap: the only way to KNOW the window held more
	// than `limit` buckets. See [capOHLCSeriesNewest].
	bars, proxied, err := s.ohlcSeriesWithAliases(hCtx, pair, interval, from, to, limit+1)
	if errors.Is(err, ErrUnknownGranularity) {
		// Shouldn't fire — handler validated the interval — but guard
		// against a future code path that wires the storage layer
		// directly. Translate to 400 for caller clarity.
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-interval",
			"Invalid interval", http.StatusBadRequest,
			"storage layer rejected the interval — file a bug")
		return
	}
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(hCtx, err) {
			s.logger.Warn("OHLCSeries deadline exceeded",
				"base", pair.Base.String(), "quote", pair.Quote.String(),
				"interval", interval, "from", from, "to", to)
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/ohlc-timeout",
				"OHLC query timed out", http.StatusServiceUnavailable,
				"the underlying CAGG didn't return in 8s; cache may still be warming.")
			return
		}
		s.logger.Error("OHLCSeries failed",
			"err", err, "base", pair.Base.String(), "quote", pair.Quote.String(),
			"interval", interval, "from", from, "to", to)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	bars = capOHLCSeriesNewest(bars, limit)
	bars = withoutVendorVolumes(bars)

	// dex-nonstandard-decimals forward normalization: K = 10^(baseDec-quoteDec) is constant for the
	// requested pair, so applying it once to the finished series (not per CAGG row) is exact, also
	// across ohlcSeriesFiatCombined's bars (all constituents share the base token and a 7dp peg).
	//
	// v_base/v_quote are NOT touched: they are raw sums at the per-source scale that
	// [barScaleDecimals] reads from `sources` and the bar states on the wire.
	baseDec := aggregate.ResolveDecimals(s.NonstandardDecimals, pair.Base)
	quoteDec := aggregate.ResolveDecimals(s.NonstandardDecimals, pair.Quote)
	bars = adjustOHLCSeriesBars(bars, baseDec, quoteDec)

	// Series mode returns 200 with empty `intervals: []` for no closed buckets, unlike the 404 of
	// single-bar, because charts expect a stable shape. The slice must be non-nil for byte-stability.
	if bars == nil {
		bars = []OHLCSeriesBar{}
	}
	resp := OHLCSeriesResponse{
		Base:      pair.Base.String(),
		Quote:     pair.Quote.String(),
		Interval:  string(interval),
		From:      WireTime(from),
		To:        WireTime(to),
		Intervals: bars,
	}
	// Triangulated fires only when a contributing bar came through a proxy constituent (a peg's
	// classic/SAC form or a stablecoin backer); a fiat series served by a directly-quoted market is
	// not triangulated just because the quote is fiat.
	flags := Flags{Triangulated: proxied}
	// An empty series is ambiguous ("quiet market" and "before anything held" are the same bytes),
	// and `to` is exclusive, so a window ending at or before the floor is provably the second
	// reading. The floor covers what ohlcSeriesWithAliases read: the pair, or a fiat quote's whole
	// constituent set.
	coverageFrom, outside := s.coverageAnnotationIfEmpty(
		r.Context(), s.ohlcCoverageSet(pair), to, len(bars) == 0)
	flags.OutsideCoverage = outside
	writeJSONCoverage(w, resp, flags, coverageFrom)
}

// capOHLCSeriesNewest trims an ascending `limit+1` probe read to the NEWEST `limit` bars and marks them `truncated` only when the probe
// row came back:
// `len(bars) == limit` proves nothing (no `from` sizes the window to exactly `limit`; a sparse market can hold exactly `limit`).
// Newest because both capped readers keep the newest rows; callers page back with `to` = the first bar's `t`. The result is a fresh slice
// (readers may own bars).
func capOHLCSeriesNewest(bars []OHLCSeriesBar, limit int) []OHLCSeriesBar {
	if limit <= 0 || len(bars) <= limit {
		return bars
	}
	out := make([]OHLCSeriesBar, limit)
	copy(out, bars[len(bars)-limit:])
	for i := range out {
		out[i].Truncated = true
	}
	return out
}

// withoutVendorVolumes blanks v_base, v_quote and n on bars fed only by non-VWAP sources: a vendor daily close is one
// carrier row per day, not fills, so its amounts are not volume. O/H/L/C and `sources` stay and state the provenance.
// The input is left untouched; a copy is made only when a bar changes.
func withoutVendorVolumes(bars []OHLCSeriesBar) []OHLCSeriesBar {
	var out []OHLCSeriesBar
	for i := range bars {
		if !vendorOnlySources(bars[i].Sources) {
			continue
		}
		if out == nil {
			out = append([]OHLCSeriesBar(nil), bars...)
		}
		out[i].VBase, out[i].VQuote, out[i].N = "0", "0", 0
	}
	if out == nil {
		return bars
	}
	return out
}

// pgTimeBucketOrigin is TimescaleDB's default time_bucket origin ([timescale.Store.OHLCSeriesReBucketed]); [time.Time.Truncate] agrees
// only for widths
// dividing the origin offset (1h/4h/12h/1d, not 3d or 2w), so a default `to` would miss the folded rows' boundaries.
var pgTimeBucketOrigin = time.Date(2000, 1, 3, 0, 0, 0, 0, time.UTC)

// truncateToBucketOrigin rounds t DOWN to a multiple of d from [pgTimeBucketOrigin], reproducing time_bucket's floor; d <= 0 is a no-op.
func truncateToBucketOrigin(t time.Time, d time.Duration) time.Time {
	if d <= 0 {
		return t
	}
	rem := t.Sub(pgTimeBucketOrigin) % d
	if rem < 0 {
		rem += d
	}
	return t.Add(-rem)
}

// parseOHLCSeriesFromTo parses from/to with interval-aware defaults: `to` snaps DOWN to an interval boundary (ADR-0015), `from` is to -
// limit*interval,
// explicit values are verbatim (UTC). ok=false after a problem+json on a parse error or from >= to.
func parseOHLCSeriesFromTo(
	w http.ResponseWriter, r *http.Request,
	interval ohlcInterval, limit int,
) (from, to time.Time, ok bool) {
	intervalDur := interval.duration()
	toExplicit := r.URL.Query().Get("to") != ""
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
	if !toExplicit {
		to = truncateToBucketOrigin(to, intervalDur)
	}

	from = to.Add(-time.Duration(limit) * intervalDur)
	if raw := r.URL.Query().Get("from"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
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

// ohlcSeriesWithAliases returns the FIRST non-empty series across the XLM dual-form alias pairs (CAGGs key bars by the id trades carried,
// e.g.
// CEX bars under `crypto:XLM`); no cross-alias fusion, matching the single-bar endpoint. The bool is `proxied` (flags.triangulated),
// always false off the fiat path.
func (s *Server) ohlcSeriesWithAliases(
	ctx context.Context,
	pair canonical.Pair,
	interval ohlcInterval,
	from, to time.Time,
	limit int,
) ([]OHLCSeriesBar, bool, error) {
	// Fiat quotes have no deep trade stream; the history lives under the pegged stablecoin pairs.
	// Combine those constituents per bucket (the live aggregator's source set) instead of first-hit,
	// which would serve only the recent direct CEX feed.
	if pair.Quote.Type == canonical.AssetFiat {
		return s.ohlcSeriesFiatCombined(ctx, pair, interval, from, to, limit)
	}
	for _, a := range assetAliases(pair.Base) {
		for _, q := range assetAliases(pair.Quote) {
			ap, perr := canonical.NewPair(a, q)
			if perr != nil {
				continue // degenerate alias combination (identity pair)
			}
			bars, err := s.History.OHLCSeries(ctx, ap, string(interval), from, to, limit)
			if err != nil || len(bars) > 0 {
				annotateOHLCSeriesBarScale(bars)
				normalizeOHLCSeriesVolumes(bars)
				return bars, false, err
			}
		}
	}
	return nil, false, nil
}

// annotateOHLCSeriesBarScale fills VBaseDecimals/VQuoteDecimals from each bar's Sources via [barScaleDecimals]. It must not share a path
// with
// [ohlcBucketAcc.finalize]: a combined bucket's scale is its constituents' lift target, not a lookup on the union of their venues.
func annotateOHLCSeriesBarScale(bars []OHLCSeriesBar) {
	for i := range bars {
		d := barScaleDecimals(bars[i].Sources)
		bars[i].VBaseDecimals = wireScaleDecimals(d)
		bars[i].VQuoteDecimals = wireScaleDecimals(d)
	}
}

// wireScaleDecimals renders a scale for the wire; unknown is null, never -1 (a consumer computing 10^-1 would scale the volume up
// tenfold).
func wireScaleDecimals(scale int) *int {
	if scale == ohlcBarScaleUnknown {
		return nil
	}
	return &scale
}

// adjustOHLCSeriesBars applies dex-nonstandard-decimals normalization to every bar (see [Server.handleOHLCSeries]); bars are returned
// UNCHANGED when
// baseDecimals == quoteDecimals, since reformatting the CAGG's NUMERIC::text via [ratToDecimal] would change bytes for every 7dp pair.
func adjustOHLCSeriesBars(bars []OHLCSeriesBar, baseDecimals, quoteDecimals int) []OHLCSeriesBar {
	if baseDecimals == quoteDecimals || len(bars) == 0 {
		return bars
	}
	out := make([]OHLCSeriesBar, len(bars))
	for i, b := range bars {
		out[i] = b
		out[i].O = adjustOHLCPriceString(b.O, baseDecimals, quoteDecimals)
		out[i].H = adjustOHLCPriceString(b.H, baseDecimals, quoteDecimals)
		out[i].L = adjustOHLCPriceString(b.L, baseDecimals, quoteDecimals)
		out[i].C = adjustOHLCPriceString(b.C, baseDecimals, quoteDecimals)
		// VBase / VQuote stay untouched: raw sums at the asset's own decimals are correct regardless.
	}
	return out
}

// adjustOHLCPriceString applies [aggregate.AdjustPrice] to a NUMERIC price string and reformats at [ohlcPriceDigits]; unparsable input
// passes through.
func adjustOHLCPriceString(s string, baseDecimals, quoteDecimals int) string {
	raw := ratFromDecimal(s)
	if raw == nil {
		return s
	}
	return ratToDecimal(aggregate.AdjustPrice(raw, baseDecimals, quoteDecimals), ohlcPriceDigits)
}
