package v1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TWAPResult is the wire shape for /v1/twap responses.
//
// Price is the time-weighted mean as a decimal string (10-digit
// precision, consistent with VWAP / OHLC). TradeCount is the number
// of trades that carried weight — priced, surviving the outlier filter,
// and in an instant with a positive slot. OutliersFiltered counts the
// trades the sigma filter removed. Truncated signals the window had
// more trades than the server's per-request cap; see
// VWAPResult.Truncated for the same semantics.
type TWAPResult struct {
	From             WireTime `json:"from"`
	To               WireTime `json:"to"`
	Price            string   `json:"price"`
	TradeCount       int      `json:"trade_count"`
	OutliersFiltered int      `json:"outliers_filtered"`
	Truncated        bool     `json:"truncated"`
}

// handleTWAP serves GET /v1/twap?base=...&quote=...&from=...&to=...
//
// Defaults match /v1/history (1-hour window ending now). TWAP
// weights each instant's price by the duration until the next instant
// (or windowEnd for the last); see internal/aggregate/twap.go for the
// formula.
//
// The outlier filter defaults ON at /v1/ohlc's sigma: a time weight is
// unrelated to trade size, so on a thin pair one dust print alone in
// its ledger carries the whole interval to the next ledger's trade.
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

	// Scam-issuer gate (wave-D MSP-02). /v1/vwap and /v1/twap served a
	// flagged issuer's aggregated price at 200 while /v1/price,
	// /v1/price/tip, /v1/price/batch, the SEP-40 oracle and the asset
	// headline all withheld it — reproduced live against a directory-
	// flagged issuer, 200 with a price on both. Worse, pricingguard's own
	// package doc (scam.go) and PR #182's merged body BOTH asserted these
	// two endpoints were covered by the reader-seam gate. They never
	// were: the ScamGate is consumed at exactly four sites, none of them
	// here, and no middleware does asset-level withholding.
	//
	// Asked about BOTH legs, via the package's one spelling of the
	// decision (scamWithheld → pricingguard's pair fold). Keyed on the
	// base alone it covered every quote but not itself as one: the price
	// of XLM in a flagged issuer's asset IS the flagged market's price,
	// inverted, so `?base=native&quote=<FLAGGED>` served the exact
	// reciprocal of the number this handler had just withheld for the
	// other orientation (F019). Both orientations name one market.
	//
	// SCAM ONLY, deliberately not the substance gate. The scam gate is
	// targeted (flagged issuers) and directly implements the 2026-08-25
	// decision. The substance gate would newly 404 every THIN pair here,
	// which is both a breaking change for existing clients and arguably
	// wrong on principle: VWAPResult's own doc and ADR-0015 position
	// /v1/vwap as the "narrow the window and compute it yourself" surface
	// OPPOSITE /v1/price. That is an owner decision, not something to
	// smuggle in with a scam fix.
	//
	// The gate goes in the HANDLER, not in the shared
	// tradesInRangeWithStablecoinFallback: that helper is also the fetch
	// behind the single-bar /v1/ohlc, and scam.go, substance.go, the
	// config docs and the withheld problem's own guidance text all
	// promise /v1/ohlc stays visible. Gating there would make our own
	// error message's escape-hatch advice a lie.
	if s.writeIfScamWithheld(w, r, base, quote, "twap") {
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
	sigma, ok := parseOHLCOutlierSigma(w, r)
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
		writeProblemErr(w, r, err,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	res, ok := s.computeTWAP(w, r, pair, from, to, trades, sigma)
	if !ok {
		return
	}
	res.Truncated = len(trades) == maxTrades
	writeJSON(w, res, Flags{Triangulated: triangulated})
}

// computeTWAP filters trades at sigma and time-weights the survivors
// over [from, to), writing the problem response itself on failure.
// Truncated is left for the caller, which owns the trade cap.
func (s *Server) computeTWAP(
	w http.ResponseWriter, r *http.Request, pair canonical.Pair,
	from, to time.Time, trades []canonical.Trade, sigma float64,
) (TWAPResult, bool) {
	pre := len(trades)
	if sigma > 0 {
		trades = aggregate.FilterOutliers(trades, sigma)
	}
	price, weighted, err := aggregate.TWAPWithCount(trades, to)
	if errors.Is(err, aggregate.ErrNoTrades) {
		if sigma > 0 && pre > 0 && len(trades) == 0 {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/all-filtered",
				"All trades filtered as outliers", http.StatusUnprocessableEntity,
				fmt.Sprintf("outlier_sigma=%v removed all %d trades in window; relax the threshold or pass outlier_sigma=0",
					sigma, pre))
			return TWAPResult{}, false
		}
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/no-trades",
			"No trades in window", http.StatusNotFound,
			"no trades observed for "+pair.Base.String()+"/"+pair.Quote.String()+
				" between "+from.Format(time.RFC3339)+" and "+to.Format(time.RFC3339))
		return TWAPResult{}, false
	}
	if err != nil {
		s.logger.Error("TWAP failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return TWAPResult{}, false
	}

	// dex-nonstandard-decimals forward normalization — see handleVWAP's
	// equivalent comment.
	price = aggregate.AdjustPrice(price,
		aggregate.ResolveDecimals(s.nonstandardDecimals, pair.Base),
		aggregate.ResolveDecimals(s.nonstandardDecimals, pair.Quote))

	return TWAPResult{
		From:             WireTime(from),
		To:               WireTime(to),
		Price:            ratToDecimal(price, ohlcPriceDigits),
		TradeCount:       weighted,
		OutliersFiltered: pre - len(trades),
	}, true
}
