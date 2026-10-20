package v1

import (
	"context"
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// handlePairs serves GET /v1/pairs?base=&quote=: a [Market] array of zero or one entries (OpenAPI PairsEnvelope), reusing
// [MarketsReader].
// A missing pair is NOT a 404, so clients tell "no such pair" from a malformed request by status.
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

	reader := s.Markets
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
		// Scam-issuer gate and nonstandard-decimals normalization (markets.go's
		// adjustListingPrice) resolve against the matched alias legs (native vs crypto:XLM vs
		// the SAC), not the requested spelling.
		market.LastPrice = s.adjustListingPrice(r.Context(), matchedBase, matchedQuote, market.LastPrice, "pairs")
		out = append(out, market)
	}
	writeJSON(w, out, Flags{})
}

// pairMarketWithAliases tries each XLM alias combination of base and quote (like [Server.ohlcSeriesWithAliases]) since a pair
// lives under whichever id its trades carried; the matched legs are returned so decimals resolve against the pair that produced
// the row.
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
