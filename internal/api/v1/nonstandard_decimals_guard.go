package v1

import (
	"fmt"
	"net/http"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/obs"
)

// declineIfNonstandardDecimals is the READ-TIME enforcement point for the
// dex-nonstandard-decimals guard
// (docs/operations/runbooks/dex-nonstandard-decimals.md), for the serving
// paths that STILL read a raw, unnormalized ratio.
//
// Confirmed production bug (2026-07-08): the served price is
// Σ(quote_amount)/Σ(base_amount) on raw smallest-unit integers, in both the
// `prices_*` continuous aggregates and `aggregate.VWAP`. Per-asset decimals
// cancel in that ratio ONLY when base and quote share a decimals scale.
// Token CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO declares
// decimals()=9, so its aquarius/USDC pair served exactly 100x wrong (41.32
// vs true ~4132) for 35 trades.
//
// Forward normalization (2026-07-10, docs/operations/runbooks/
// dex-nonstandard-decimals.md "Root cause analysis") replaced the decline
// with a real corrected price on every QUERY-TIME (raw-trade) serving
// path — /v1/vwap, /v1/twap, /v1/history, /v1/price/tip, and /v1/ohlc's
// single-bar mode all call [aggregate.AdjustPrice] instead of this
// function now. What remains gated here are the paths that read a
// `prices_*` / `twap_*` continuous aggregate — a materialized, ALREADY-
// unnormalized ratio that this fix deliberately did not rewrite (CAGG
// rebuild risk; see the runbook) — namely /v1/price (its closed-1m-bucket
// path) and /v1/ohlc's multi-bar `interval=` series mode. Both are called
// immediately after asset/quote parsing, before any storage read. Returns
// true when it wrote a 422 problem+json decline; the caller MUST return
// immediately without serving any price. Nil-safe: a deployment with no
// NonstandardDecimalsCache wired (s.nonstandardDecimals == nil) always
// returns false — the guard is opt-in and fails open by construction (see
// [NonstandardDecimalsCache.Lookup]).
func (s *Server) declineIfNonstandardDecimals(w http.ResponseWriter, r *http.Request, base, quote canonical.Asset) bool {
	if s.nonstandardDecimals == nil {
		return false
	}
	leg, decimals, flagged := nonstandardDecimalsLeg(s.nonstandardDecimals, base, quote)
	if !flagged {
		return false
	}
	obs.PriceServeDeclinedNonstandardDecimalsTotal.WithLabelValues(leg).Inc()
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/nonstandard-decimals",
		"Pricing temporarily unavailable for a non-standard-decimals asset",
		http.StatusUnprocessableEntity,
		fmt.Sprintf(
			"%s declares on-chain decimals()=%d, not the assumed 7 — the served price for "+
				"pairs involving it is not yet decimals-normalized, so pricing is declined "+
				"rather than served skewed by 10^(7-decimals). This is temporary: it self-clears "+
				"once decimals normalization ships. See docs/operations/runbooks/dex-nonstandard-decimals.md.",
			leg, decimals,
		),
	)
	return true
}

// nonstandardDecimalsLeg checks base then quote against the cache and
// returns the first flagged leg. Both legs flagged is possible in
// principle (two offending Soroban tokens traded directly against each
// other) — base wins arbitrarily since the response only needs to name
// one to be actionable; the guard still declines either way.
func nonstandardDecimalsLeg(cache *NonstandardDecimalsCache, base, quote canonical.Asset) (asset string, decimals int, flagged bool) {
	if d, ok := cache.Lookup(base.String()); ok {
		return base.String(), d, true
	}
	if d, ok := cache.Lookup(quote.String()); ok {
		return quote.String(), d, true
	}
	return "", 0, false
}
