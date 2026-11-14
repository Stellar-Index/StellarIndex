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

	market, found, err := s.pairMarketWithAliases(r.Context(), reader, base, quote)
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
		out = append(out, market)
	}
	writeJSON(w, out, Flags{})
}

// pairMarketWithAliases answers one market for a pair that is stored under several spellings. The first alias combination that
// traded leads; every other combination that /v1/markets folds into the same row (a SAC spelling onto its classic or native form,
// crypto:XLM kept apart as the CEX population) is added to it, so the point query and the listing agree.
func (s *Server) pairMarketWithAliases(
	ctx context.Context, reader MarketsReader, base, quote canonical.Asset,
) (Market, bool, error) {
	var (
		out   Market
		lead  canonical.Pair
		found bool
		seen  []canonical.Pair
	)
	for _, b := range assetAliases(base) {
		for _, q := range assetAliases(quote) {
			ap, perr := canonical.NewPair(b, q)
			if perr != nil {
				continue // degenerate alias combination (identity pair)
			}
			folded, perr := canonical.NewPair(sacFolded(b), sacFolded(q))
			if perr != nil {
				folded = ap // an asset against its own SAC is a market of its own
			}
			if (found && !folded.Equal(lead)) || pairSeen(seen, ap) {
				continue
			}
			seen = append(seen, ap) // PairMarket reads both stored directions: a flip is the same rows
			market, ok, err := reader.PairMarket(ctx, ap.Base, ap.Quote)
			if err != nil {
				return Market{}, false, err
			}
			if !ok {
				continue
			}
			// Scam gate and decimals resolve against the spelling that produced the row.
			market.LastPrice = s.adjustListingPrice(ctx, ap.Base, ap.Quote, market.LastPrice, "pairs")
			if !found {
				out, lead, found = market, folded, true
				out.Base, out.Quote = folded.Base.String(), folded.Quote.String()
				continue
			}
			foldPairSpelling(&out, market)
		}
	}
	return out, found, nil
}

// sacFolded maps a SAC spelling to its classic or native form, the key /v1/markets folds rows on; any other asset is itself.
func sacFolded(a canonical.Asset) canonical.Asset {
	if a.Type == canonical.AssetSoroban {
		return canonical.CanonicalAsset(a)
	}
	return a
}

func pairSeen(seen []canonical.Pair, p canonical.Pair) bool {
	for _, k := range seen {
		if k.EqualEitherWay(p) {
			return true
		}
	}
	return false
}

// foldPairSpelling adds another stored spelling of the same market into dst: counts and volume sum, and the newer trade's
// timestamp and price win (an older spelling's price is kept only when the newer one has none).
func foldPairSpelling(dst *Market, m Market) {
	dst.TradeCount24h += m.TradeCount24h
	dst.Volume24hUSD = addDecimalStrings(dst.Volume24hUSD, m.Volume24hUSD)
	dst.VolumeLowerBound = dst.VolumeLowerBound || m.VolumeLowerBound
	if m.LastTradeAt.After(dst.LastTradeAt) {
		dst.LastTradeAt, dst.BucketCloseAt = m.LastTradeAt, m.BucketCloseAt
		if m.LastPrice != nil {
			dst.LastPrice = m.LastPrice
		}
	} else if dst.LastPrice == nil {
		dst.LastPrice = m.LastPrice
	}
}
