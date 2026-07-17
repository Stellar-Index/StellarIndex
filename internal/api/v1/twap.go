package v1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TWAPResult is the wire shape for /v1/twap responses.
//
// Price is the time-weighted mean as a decimal string (10-digit
// precision, consistent with VWAP / OHLC). TradeCount is the number
// of trades that contributed to the weighting. Truncated signals
// the window had more trades than the server's per-request cap;
// see VWAPResult.Truncated for the same semantics.
type TWAPResult struct {
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	Price      string    `json:"price"`
	TradeCount int       `json:"trade_count"`
	Truncated  bool      `json:"truncated"`
}

// handleTWAP serves GET /v1/twap?base=...&quote=...&from=...&to=...
//
// Defaults match /v1/history (1-hour window ending now). TWAP
// weights each trade's price by the duration until the next trade
// (or windowEnd for the final trade); see internal/aggregate/twap.go
// for the formula.
//
// No outlier_sigma param on TWAP — time-weighting is itself a form
// of outlier resistance (a single spurious print that corrects
// 1 second later has 1-second weight, not a full window's worth).
func (s *Server) handleTWAP(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/twap-unavailable",
			"TWAP serving not configured", http.StatusServiceUnavailable,
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

	// dex-nonstandard-decimals: like /v1/vwap, /v1/twap computes entirely
	// from raw trades at query time (no CAGG involved), so the price is
	// normalized below via aggregate.AdjustPrice instead of declined.

	// Clamped to a closed-bucket boundary when `to` defaults to "now"
	// per ADR-0015.
	from, to, _, ok := parseFromToClamped(w, r)
	if !ok {
		return
	}

	// Per-request DB ceiling (P1/C3-2, audit-2026-07-16): /v1/twap
	// scans raw `trades` on every query — same posture as /v1/vwap.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	const maxTrades = 10000
	trades, triangulated, err := s.tradesInRangeWithStablecoinFallback(ctx, pair, from, to, maxTrades)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("TradesInRange failed for TWAP",
			"err", err, "base", base.String(), "quote", quote.String())
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	price, err := aggregate.TWAP(trades, to)
	if errors.Is(err, aggregate.ErrNoTrades) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/no-trades",
			"No trades in window", http.StatusNotFound,
			"no trades observed for "+pair.Base.String()+"/"+pair.Quote.String()+
				" between "+from.Format(time.RFC3339)+" and "+to.Format(time.RFC3339))
		return
	}
	if err != nil {
		s.logger.Error("TWAP failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	// dex-nonstandard-decimals forward normalization — see handleVWAP's
	// equivalent comment.
	price = aggregate.AdjustPrice(price,
		aggregate.ResolveDecimals(s.nonstandardDecimals, base),
		aggregate.ResolveDecimals(s.nonstandardDecimals, quote))

	writeJSON(w, TWAPResult{
		From:       from,
		To:         to,
		Price:      ratToDecimal(price, ohlcPriceDigits),
		TradeCount: len(trades),
		Truncated:  len(trades) == maxTrades,
	}, Flags{Triangulated: triangulated})
}
