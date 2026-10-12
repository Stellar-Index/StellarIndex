package v1

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// handleObservations serves GET /v1/observations (ADR-0018 Surface 3): one TradeRow per source that ever traded (asset, quote); empty
// array, not 404.
// Params: asset (required), quote (default fiat:USD), source (on-chain only, else 400 via rawTradeSourceFilterOK), aggregate=latest.
// FRESHNESS CONTRACT: flags.stale is always false, freeze/divergence are not consulted, and ?granularity=/?window_seconds= give 400.
func (s *Server) handleObservations(w http.ResponseWriter, r *http.Request) {
	if s.History == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/observations-unavailable",
			"Observations serving not configured", http.StatusServiceUnavailable,
			"this deployment has no HistoryReader wired — check binary configuration")
		return
	}

	if !rejectObservationsTierParams(w, r) {
		return
	}

	asset, quote, ok := parseObservationsAssetQuote(w, r)
	if !ok {
		return
	}
	pair, err := canonical.NewPair(asset, quote)
	if err != nil {
		// Identity-pair was already rejected upstream; any other
		// validation error here is unexpected. Surface as 400.
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-pair",
			"Invalid pair", http.StatusBadRequest, err.Error())
		return
	}

	source := r.URL.Query().Get("source")
	if !rawTradeSourceFilterOK(w, r, source) {
		return
	}

	// aggregate is currently single-valued ("latest"); reject anything
	// else as a 400 rather than ignoring — keeps the surface honest.
	aggregate := r.URL.Query().Get("aggregate")
	if aggregate != "" && aggregate != "latest" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-aggregate",
			"Invalid aggregate parameter", http.StatusBadRequest,
			`aggregate must be "latest" or omitted`)
		return
	}

	// No quote family is short-circuited to an empty answer, `fiat:USD` least of all: it is the
	// default quote AND a real stored trade quote (CEX connectors trade straight against it), so
	// `[]` would contradict /v1/history and /v1/observations/stream. Cost needs no fast-path
	// either: trades_pair_source_ts_idx covers LatestTradePerSource, so the lookup is
	// O(num_sources).

	// 8s ceiling on the trades hypertable scan, as /v1/pools and /v1/markets: a cold-cache "latest
	// trade per source" scan over a busy pair can take several seconds, and the bound yields a
	// structured 503 instead of holding the connection until the upstream LB cuts it.
	obsCtx, obsCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer obsCancel()
	trades, ok := s.fetchObservationsOrWriteError(w, r, obsCtx, pair, source, aggregate, asset, quote)
	if !ok {
		return
	}

	rows := make([]TradeRow, len(trades))
	srcSet := make(map[string]struct{}, len(trades))
	for i, t := range trades {
		rows[i] = tradeRowFrom(t, 0) // 0 → default 10 fractional digits
		srcSet[t.Source] = struct{}{}
	}
	// dex-nonstandard-decimals forward normalization of each row's Price (M2),
	// shared with /v1/history via normalizeTradeRowPrices. Byte-identical no-op
	// for a pair with no confirmed non-7-decimals leg.
	s.normalizeTradeRowPrices(rows, trades, pair.Base, pair.Quote)
	srcs := make([]string, 0, len(srcSet))
	for src := range srcSet {
		srcs = append(srcs, src)
	}
	// Map iteration is unordered; sort so the `sources` array is
	// byte-identical across requests (the response-equality contract).
	sort.Strings(srcs)

	// Single-source flag: true when exactly one source contributed (informational).
	//
	// FRESHNESS CONTRACT: this RAW per-source surface has no single staleness verdict.
	// flags.stale is always false BY DESIGN and the envelope's as_of is the response time, NOT the
	// data's age; consumers assess freshness per row from its own trade timestamp.
	//
	// divergence_checked is also structurally false and s.Divergence is never consulted: the
	// cross-reference verdict is cached per base and speaks to an aggregated number, so stamping
	// it on each venue's last trade would claim a cross-check that never happened. Pinned by
	// TestObservations_DivergenceCheckedStructurallyFalse; the stream twin
	// (observationsStreamEvent) mirrors it.
	flags := Flags{SingleSource: len(srcs) == 1}

	// Triangulation hint: an empty array reads as "unpriced" when /v1/price serves the pair via
	// triangulation or the stablecoin-fiat proxy and only no source has a DIRECT trade (this
	// surface is raw-only, ADR-0018). Set flags.triangulated=true in that case, as /v1/price does.
	if len(rows) == 0 && source == "" {
		flags.Triangulated = s.observationsHaveTriangulatedPrice(obsCtx, pair)
	}

	writeJSON(w, rows, flags, srcs...)
}

// fetchObservationsOrWriteError runs computeObservations and maps errors to problem+json (client abort silent, timeout/cache-unavailable
// 503, else 500).
// (_, false) means a response was already written.
func (s *Server) fetchObservationsOrWriteError(
	w http.ResponseWriter, r *http.Request,
	obsCtx context.Context,
	pair canonical.Pair, source, aggregate string,
	asset, quote canonical.Asset,
) ([]canonical.Trade, bool) {
	trades, err := s.computeObservations(obsCtx, pair, source, aggregate)
	if err == nil {
		return trades, true
	}
	if clientAborted(r, err) {
		return nil, false
	}
	if handlerTimedOut(obsCtx, err) {
		s.logger.Warn("computeObservations deadline exceeded",
			"asset", asset.String(), "quote", quote.String(), "source", source)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/observations-timeout",
			"Observations query timed out", http.StatusServiceUnavailable,
			"the trades hypertable scan didn't return in 8s; retry shortly.")
		return nil, false
	}
	if IsCacheUnavailable(err) {
		s.logger.Warn("computeObservations cache unavailable",
			"err", err, "asset", asset.String(), "quote", quote.String(),
			"source", source)
		writeCacheUnavailableProblem(w, r)
		return nil, false
	}
	s.logger.Error("LatestTradePerSource failed",
		"err", err, "asset", asset.String(), "quote", quote.String(),
		"source", source)
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/internal",
		"Internal error", http.StatusInternalServerError, "")
	return nil, false
}

// observationsHaveTriangulatedPrice is the best-effort check behind the empty-observations hint (would /v1/price serve the pair via Redis
// VWAP
// or the stablecoin-fiat proxy); errors downgrade to false since it is a UX hint only.
func (s *Server) observationsHaveTriangulatedPrice(ctx context.Context, pair canonical.Pair) bool {
	if _, _, _, _, ok := s.tryRedisVWAPFallback(ctx, pair.Base, pair.Quote); ok {
		return true
	}
	if _, _, ok, _ := s.tryStablecoinFiatProxy(ctx, pair.Base, pair.Quote); ok {
		return true
	}
	return false
}

// rejectObservationsTierParams enforces ADR-0018 URL discipline: ?granularity=/?window_seconds= would silently change the consistency
// contract, so 400.
func rejectObservationsTierParams(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	if q.Get("granularity") != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-observations-param",
			"granularity is not valid on /v1/observations", http.StatusBadRequest,
			"granularity is a closed-bucket concept (ADR-0018); /v1/observations is raw per-source")
		return false
	}
	if q.Get("window_seconds") != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-observations-param",
			"window_seconds is not valid on /v1/observations", http.StatusBadRequest,
			"window_seconds is a tip-surface concept (ADR-0018); /v1/observations does not aggregate")
		return false
	}
	return true
}

// parseObservationsAssetQuote mirrors parseTipAssetQuote but stays separate so each surface keeps its own problem+json error type URLs.
func parseObservationsAssetQuote(w http.ResponseWriter, r *http.Request) (canonical.Asset, canonical.Asset, bool) {
	rawAsset, ok := resolveAssetOrBaseParam(w, r)
	if !ok {
		return canonical.Asset{}, canonical.Asset{}, false
	}
	asset, err := canonical.ParseAsset(rawAsset)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset identifier", http.StatusBadRequest, err.Error())
		return canonical.Asset{}, canonical.Asset{}, false
	}
	quote := defaultPriceQuote
	if raw := r.URL.Query().Get("quote"); raw != "" {
		q, err := canonical.ParseAsset(raw)
		if err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-quote",
				"Invalid quote identifier", http.StatusBadRequest, err.Error())
			return canonical.Asset{}, canonical.Asset{}, false
		}
		quote = q
	}
	if asset.Equal(quote) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/identity-price",
			"Asset and quote are the same", http.StatusBadRequest,
			"price of an asset in itself is always 1; parameters must differ")
		return canonical.Asset{}, canonical.Asset{}, false
	}
	return asset, quote, true
}

// collapseToLatest returns the newest trade by Timestamp (ledger breaks ties) for aggregate=latest, keeping the array wire shape.
func collapseToLatest(trades []canonical.Trade) []canonical.Trade {
	if len(trades) == 0 {
		return trades
	}
	bestIdx := 0
	for i := 1; i < len(trades); i++ {
		if isLater(trades[i], trades[bestIdx]) {
			bestIdx = i
		}
	}
	return []canonical.Trade{trades[bestIdx]}
}

// isLater reports whether a is newer than b: Timestamp first, then ledger (Stellar packs many trades into one ledger).
func isLater(a, b canonical.Trade) bool {
	if a.Timestamp.After(b.Timestamp) {
		return true
	}
	if a.Timestamp.Before(b.Timestamp) {
		return false
	}
	return a.Ledger > b.Ledger
}
