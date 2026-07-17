package v1

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// VWAPResult is the wire shape for /v1/vwap responses.
//
// Price is the volume-weighted mean as a decimal string (10-digit
// precision, consistent with /v1/history + /v1/ohlc). Volumes are
// raw integer strings in the asset's smallest unit.
//
// OutliersFiltered reports how many trades the sigma filter
// removed before the VWAP computation — zero when outlier_sigma=0
// was passed or there weren't enough samples for the filter.
//
// Truncated is true when the window had MORE than the server's
// max-trades cap (10000 today) — the returned Price is then only
// over the chronologically-first 10000 trades and is NOT the true
// window VWAP. Clients should narrow the window and retry. For
// fixed cross-region-consistent VWAPs, `/v1/price` serves the
// closed-bucket aggregator output instead (per ADR-0015).
type VWAPResult struct {
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	Price            string    `json:"price"`
	BaseVolume       string    `json:"base_volume"`
	QuoteVolume      string    `json:"quote_volume"`
	TradeCount       int       `json:"trade_count"`
	OutliersFiltered int       `json:"outliers_filtered"`
	Truncated        bool      `json:"truncated"`
}

// handleVWAP serves GET /v1/vwap?base=...&quote=...&from=...&to=...&outlier_sigma=...
//
// Defaults match /v1/history (1-hour window ending now). outlier_sigma
// defaults to 0 (no filtering) — the aggregator's config-default of
// 4σ is a different layer's decision.
func (s *Server) handleVWAP(w http.ResponseWriter, r *http.Request) {
	reader := s.history
	if reader == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/vwap-unavailable",
			"VWAP serving not configured", http.StatusServiceUnavailable,
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

	// dex-nonstandard-decimals: /v1/vwap computes entirely from raw
	// trades at query time (no CAGG involved), so — unlike /v1/price and
	// /v1/ohlc's multi-bar series mode — it no longer needs the decline
	// guard. The price is normalized below via aggregate.AdjustPrice
	// instead of declined. See docs/operations/runbooks/
	// dex-nonstandard-decimals.md "Root cause analysis".

	// Clamped to a closed-bucket boundary when `to` defaults to "now"
	// per ADR-0015 — guarantees cross-region answer agreement.
	from, to, _, ok := parseFromToClamped(w, r)
	if !ok {
		return
	}

	sigma := 0.0
	if raw := r.URL.Query().Get("outlier_sigma"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		// NaN comparisons are always false, so `v < 0` doesn't catch
		// ParseFloat("NaN"). Also reject ±Inf explicitly.
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-sigma",
				"Invalid outlier_sigma", http.StatusBadRequest,
				"outlier_sigma must be a non-negative finite number; omit or 0 disables filtering")
			return
		}
		sigma = v
	}

	// maxTrades caps each single-shot aggregation. Hitting the cap
	// means the computed VWAP is only over the first N trades and
	// we flag the response truncated=true. /v1/vwap takes arbitrary
	// time windows, so it always scans trades on-query — the
	// aggregator binary's pre-computed rollups feed `/v1/price`'s
	// closed-bucket surface (ADR-0015), not this endpoint.
	// Per-request DB ceiling (P1/C3-2, audit-2026-07-16): /v1/vwap
	// scans raw `trades` on every query (no CAGG), so without a
	// bounded context a slow scan holds its pool connection until the
	// client gives up. 8s matches the sibling raw-scan endpoints
	// (liquidity_pools.go, markets.go) and fires before the blanket
	// request-timeout middleware.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	const maxTrades = 10000
	trades, triangulated, ok := s.fetchVWAPTrades(ctx, w, r, pair, from, to, maxTrades)
	if !ok {
		return
	}

	pre := len(trades)
	if sigma > 0 {
		trades = aggregate.FilterOutliers(trades, sigma)
	}
	outliersFiltered := pre - len(trades)

	price, err := aggregate.VWAP(trades)
	if errors.Is(err, aggregate.ErrNoTrades) {
		// Distinguish two failure modes — the wire message drives
		// client behaviour (retry with different window vs retry
		// with different sigma), so misleading it is a bug.
		if pre > 0 {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/all-filtered",
				"All trades filtered as outliers", http.StatusUnprocessableEntity,
				fmt.Sprintf("outlier_sigma=%v removed all %d trades in window; relax the threshold or omit outlier_sigma",
					sigma, pre))
			return
		}
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/no-trades",
			"No trades in window", http.StatusNotFound,
			"no trades observed for "+pair.Base.String()+"/"+pair.Quote.String()+
				" between "+from.Format(time.RFC3339)+" and "+to.Format(time.RFC3339))
		return
	}
	if err != nil {
		s.logger.Error("VWAP failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	// dex-nonstandard-decimals forward normalization: scale the raw
	// Σquote/Σbase ratio by the confirmed per-leg decimals factor. A nil
	// s.nonstandardDecimals or an unflagged pair resolves both legs to
	// aggregate.StandardDecimals, making this an exact no-op.
	price = aggregate.AdjustPrice(price,
		aggregate.ResolveDecimals(s.nonstandardDecimals, base),
		aggregate.ResolveDecimals(s.nonstandardDecimals, quote))

	writeJSON(w, VWAPResult{
		From:             from,
		To:               to,
		Price:            ratToDecimal(price, ohlcPriceDigits),
		BaseVolume:       aggregate.TotalBaseVolume(trades).String(),
		QuoteVolume:      aggregate.TotalQuoteVolume(trades).String(),
		TradeCount:       len(trades),
		OutliersFiltered: outliersFiltered,
		Truncated:        pre == maxTrades,
	}, Flags{Triangulated: triangulated})
}

// fetchVWAPTrades is the trade-fetch + error-dispatch wrapper extracted
// from handleVWAP. Returns (trades, triangulated, ok); ok=false means a
// problem+json has already been written (client-abort, cache-unavailable,
// or generic internal error) and the caller must return immediately.
// Pulled out to keep handleVWAP under the funlen budget while preserving
// the cache-unavailable branch added for F-0089.
func (s *Server) fetchVWAPTrades(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
	pair canonical.Pair, from, to time.Time, maxTrades int,
) ([]canonical.Trade, bool, bool) {
	trades, triangulated, err := s.tradesInRangeWithStablecoinFallback(ctx, pair, from, to, maxTrades)
	if err == nil {
		return trades, triangulated, true
	}
	if clientAborted(r, err) {
		return nil, false, false
	}
	if IsCacheUnavailable(err) {
		s.logger.Warn("TradesInRange cache unavailable for VWAP",
			"err", err, "base", pair.Base.String(), "quote", pair.Quote.String())
		writeCacheUnavailableProblem(w, r)
		return nil, false, false
	}
	s.logger.Error("TradesInRange failed for VWAP",
		"err", err, "base", pair.Base.String(), "quote", pair.Quote.String(),
		"from", from, "to", to)
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/internal",
		"Internal error", http.StatusInternalServerError, "")
	return nil, false, false
}

// tradesInRangeWithStablecoinFallback wraps HistoryReader.TradesInRange
// with the same X/fiat:USD → X/<peg> retry shape used in the chart
// handler (chartStablecoinFallback) and price handlers
// (tryStablecoinFiatProxy). When the literal pair has zero trades AND
// quote is fiat:USD AND the operator has declared classic USD pegs,
// re-runs against each peg in priority order; first non-empty result
// wins. triangulated=true when the fallback fired so callers can stamp
// flags.triangulated.
//
// Without this, /v1/vwap and /v1/twap 404 with "no trades in window"
// for any X/fiat:USD query out-of-the-box — same root cause as #1217.
// Used by handleVWAP + handleTWAP. The CAGG-reading siblings live
// elsewhere: ohlc_fiat_combine.go combines the peg constituents for
// the OHLC series path, and price_at.go's
// lookupPriceAtStablecoinFallback covers the closed-1m-VWAP-CAGG
// point lookup.
func (s *Server) tradesInRangeWithStablecoinFallback(
	ctx context.Context, pair canonical.Pair, from, to time.Time, maxTrades int,
) ([]canonical.Trade, bool, error) {
	trades, err := s.history.TradesInRange(ctx, pair, from, to, maxTrades)
	if err != nil {
		return nil, false, err
	}
	if len(trades) > 0 {
		return trades, false, nil
	}
	if pair.Quote.Type != canonical.AssetFiat || pair.Quote.Code != "USD" {
		return trades, false, nil
	}
	for _, peg := range s.usdPeggedClassics {
		if peg.Equal(pair.Base) {
			continue
		}
		proxied, err := canonical.NewPair(pair.Base, peg)
		if err != nil {
			continue
		}
		pp, err := s.history.TradesInRange(ctx, proxied, from, to, maxTrades)
		if err != nil || len(pp) == 0 {
			continue
		}
		return pp, true, nil
	}
	return trades, false, nil
}
