package v1

import (
	"context"
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// handlePairs serves GET /v1/pairs?base=<id>&quote=<id>.
//
// Returns a [Market] array containing zero or one entries:
// the activity summary for the requested pair if any trade has
// been seen, otherwise an empty array. The 0-or-1-element array
// shape matches the OpenAPI PairsEnvelope (data: array of
// MarketRow) — a missing pair is NOT a 404, it's an empty list,
// so clients can distinguish "no such pair" from a malformed
// request without branching on status code.
//
// Reuses the [MarketsReader] interface — /v1/pairs and
// /v1/markets share the same storage shape, just with different
// access patterns (full pageable scan vs single-pair lookup).
func (s *Server) handlePairs(w http.ResponseWriter, r *http.Request) {
	rawBase := r.URL.Query().Get("base")
	if rawBase == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-base",
			"Missing base parameter", http.StatusBadRequest,
			"base query parameter is required")
		return
	}
	base, err := canonical.ParseAsset(rawBase)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid base identifier", http.StatusBadRequest,
			err.Error())
		return
	}

	rawQuote := r.URL.Query().Get("quote")
	if rawQuote == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-quote",
			"Missing quote parameter", http.StatusBadRequest,
			"quote query parameter is required")
		return
	}
	quote, err := canonical.ParseAsset(rawQuote)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-quote",
			"Invalid quote identifier", http.StatusBadRequest,
			err.Error())
		return
	}

	if base.Equal(quote) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/identity-pair",
			"Base and quote are the same", http.StatusBadRequest,
			"a pair must have distinct base and quote assets")
		return
	}

	reader := s.markets
	if reader == nil {
		// Mirror /v1/markets's degradation: empty list instead of 503
		// so clients can integrate against the wire contract before a
		// reader is wired.
		writeJSON(w, []Market{}, Flags{})
		return
	}

	market, matchedBase, matchedQuote, found, err := s.pairMarketWithAliases(r.Context(), reader, base, quote)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("PairMarket failed",
			"err", err,
			"base", base.String(),
			"quote", quote.String())
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	out := []Market{}
	if found {
		// dex-nonstandard-decimals forward normalization — see
		// markets.go's adjustListingPrice / handlePools/handleMarkets
		// equivalent comment. Resolve decimals against the ACTUAL matched
		// alias legs (native vs crypto:XLM vs the SAC), not the requested
		// spelling — same precedent as lookupPriceAt's post-alias resolve.
		market.LastPrice = s.adjustListingPrice(matchedBase, matchedQuote, market.LastPrice)
		out = append(out, market)
	}
	writeJSON(w, out, Flags{})
}

// pairMarketWithAliases resolves the single-pair market trying each XLM
// dual-form alias combination of base and quote (F-1340) and returning
// the FIRST form with a row — the same first-hit gate
// [Server.ohlcSeriesWithAliases] applies on the series side. An
// XLM-flavoured pair lives under whichever id the contributing trades
// carried (native vs crypto:XLM vs the SAC), so a literal lookup on the
// requested spelling alone returns an empty list for a valid-but-aliased
// input. The matched legs are returned so the caller resolves
// dex-nonstandard-decimals against the pair that actually produced the row.
func (s *Server) pairMarketWithAliases(
	ctx context.Context, reader MarketsReader, base, quote canonical.Asset,
) (Market, canonical.Asset, canonical.Asset, bool, error) {
	for _, b := range assetAliases(base) {
		for _, q := range assetAliases(quote) {
			ap, perr := canonical.NewPair(b, q)
			if perr != nil {
				continue // degenerate alias combination (identity pair)
			}
			market, found, err := reader.PairMarket(ctx, ap.Base, ap.Quote)
			if err != nil {
				return Market{}, base, quote, false, err
			}
			if found {
				return market, ap.Base, ap.Quote, true, nil
			}
		}
	}
	return Market{}, base, quote, false, nil
}
