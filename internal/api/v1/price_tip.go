package v1

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// Tip-surface tunables per ADR-0018.
//
// The min/max clamp keeps the rolling-window scan cheap on a hot pair and keeps the surface a
// "tip", not a small-window historical aggregator (that is /v1/vwap's job).
const (
	defaultTipWindowSeconds = 5
	minTipWindowSeconds     = 1
	maxTipWindowSeconds     = 60

	// tipWindowMaxTrades caps the rows loaded into memory for one tip VWAP. Mirrors /v1/vwap.
	tipWindowMaxTrades = 10000

	// tipEscalationWindowSeconds is the widened retry window for an empty caller window: falling to the closed bucket (60-113s stale) would
	// breach the 30s
	// freshness SLA. It is the SLA bound, hence not configurable.
	tipEscalationWindowSeconds = 30
)

// handlePriceTip serves GET /v1/price/tip (ADR-0018): window VWAP, else last-good LatestPrice as-is with no age cap (the caller reads
// observed_at).
// flags.stale is always false and freeze stays unset; ?granularity= is rejected 400 so a stray query cannot change the surface's
// contract.
// The 5 s rolling window legitimately differs ~0.1-0.2% from /v1/price's closed 1 m bucket.
func (s *Server) handlePriceTip(w http.ResponseWriter, r *http.Request) {
	// PriceReader is the fallback path; without it the tip cannot degrade. The rolling-window path
	// needs HistoryReader, but either one alone still serves.
	if s.Prices == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Price serving not configured", http.StatusServiceUnavailable,
			"this deployment has no PriceReader wired — check binary configuration")
		return
	}

	// Reject URL-discipline violations BEFORE asset/quote parsing: mixing tip and closed-bucket
	// semantics is malformed whether or not the asset/quote parse.
	if r.URL.Query().Get("granularity") != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-tip-param",
			"granularity is not valid on /v1/price/tip", http.StatusBadRequest,
			"granularity is a closed-bucket concept (ADR-0018); use /v1/price for closed-bucket VWAP")
		return
	}

	asset, quote, ok := s.parseTipAssetQuote(w, r)
	if !ok {
		return
	}

	window, ok := parseTipWindowSeconds(w, r)
	if !ok {
		return
	}

	snapshot, sources, err := s.computeTip(r.Context(), asset, quote, window)
	if errors.Is(err, ErrPriceWithheld) {
		writePriceWithheldProblem(w, r, asset, quote, priceWithheldReason(err))
		return
	}
	if errors.Is(err, ErrPriceNotFound) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-not-found",
			"No price data for pair", http.StatusNotFound,
			"no trades or oracle observations for "+asset.String()+" / "+quote.String())
		return
	}
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if IsCacheUnavailable(err) {
			s.logger.Warn("computeTip cache unavailable",
				"err", err, "asset", asset.String(), "quote", quote.String())
			writeCacheUnavailableProblem(w, r)
			return
		}
		s.logger.Error("computeTip failed",
			"err", err, "asset", asset.String(), "quote", quote.String())
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	writeJSON(w, snapshot, s.tipFlags(r.Context(), snapshot, asset, quote, sources), sources...)
}

// tipFlags builds the flags for one tip emission, shared by the GET and both stream producers so a tip_update carries exactly what the
// GET would serve.
// Stale stays false (PriceReader's bit is ignored); divergence flags come from the requested (base, quote) spelling only, never ORed
// across other quotes or aliases.
func (s *Server) tipFlags(ctx context.Context, snap PriceSnapshot, asset, quote canonical.Asset, sources []string) Flags {
	flags := Flags{SingleSource: marketSingleSource(snap, sources), ProxyDeviation: snap.ProxyDeviation}
	flags.DivergenceWarning, flags.DivergenceChecked = s.lookupDivergenceFlag(ctx, asset, quote, 0)
	return flags
}

// computeTip is the shared core of [Server.handlePriceTip] and [Server.handlePriceTipStream]: window VWAP, then LatestPrice, then the
// Redis VWAP cache for proxy-rewritten pairs.
// ErrPriceNotFound means no branch produced a snapshot; any other error is surfaced as-is for 500 mapping.
func (s *Server) computeTip(ctx context.Context, asset, quote canonical.Asset, windowSeconds int) (PriceSnapshot, []string, error) {
	// Withholding gates run before any read: the tip is still an aggregated price claim, so a
	// dust-authored market would otherwise serve its attacker-written rate after /v1/price withheld
	// it.
	//
	// No best-effort sub-budget like the divergence lookup ([tipStreamDivergenceBudget]): timing
	// out a gate would republish the rate it withholds, so a slow gate correctly costs the
	// emission.
	//
	// [withheldBy] folds both gates, scam first. It asks about BOTH legs via [scamWithheld]:
	// `?asset=native&quote=<FLAGGED>` is the flagged market's own price inverted.
	if w := withheldBy(ctx, s.Substance, s.Scam, asset, quote, "tip"); w != pricingguard.NotWithheld {
		return PriceSnapshot{}, nil, PriceWithheldError(w)
	}
	// Which alias combinations the window merges, and which it holds back
	// until every other read has missed, is [tipMergePairs].
	merge, last := tipMergePairs(asset, quote)
	if snap, sources, ok := s.tipWindowEscalating(ctx, asset, quote, windowSeconds, merge); ok {
		return snap, sources, nil
	}
	snap, sources, ok, err := s.tipFallback(ctx, asset, quote)
	if err != nil {
		return PriceSnapshot{}, nil, err
	}
	if ok {
		// window_seconds names the caller's rolling window ([1,60]); a fallback has its source's
		// resolution instead (60s bucket, 300s cache), so it reports none.
		snap.WindowSeconds = 0
		return snap, sources, nil
	}
	// Last of all: the SAC-form combinations the caller did not name (for a wrapped classic, its
	// Soroban SAC/SAC pool). Every established read has missed, so the alternative is no price;
	// this is the SAC-last shape of canonical.AssetAliases applied to the one walk that merges.
	if snap, sources, ok = s.tipWindowEscalating(ctx, asset, quote, windowSeconds, last); ok {
		return snap, sources, nil
	}
	return PriceSnapshot{}, nil, ErrPriceNotFound
}

// tipFallback is every non-window read of [Server.computeTip], in precedence order: closed bucket, Redis VWAP cache, stablecoin-fiat
// proxy, fiat cross-rate, USD-anchored cross.
// (_, _, false, nil) is a miss; an error is a verdict to surface.
func (s *Server) tipFallback(ctx context.Context, asset, quote canonical.Asset) (PriceSnapshot, []string, bool, error) {
	// Fallback: most-recent known observation; price_type "last_trade" and "vwap" are both
	// in-contract. Route through the XLM alias loop exactly as handlePrice does, so ?asset=native
	// resolves a fresh crypto:XLM observation rather than missing it on the literal form.
	snap, sources, _, err := s.readPriceWithAliases(ctx, s.Prices, asset, quote)
	if err == nil {
		// Forward-normalize dex-nonstandard decimals: this fallback returns the RAW ratio, so a
		// confirmed non-7-decimals asset would otherwise serve a skewed tip. tipWindowVWAP normalizes
		// on its own; the Redis/proxy/fiat branches below self-normalize at their source.
		s.normalizeRawPriceSnapshot(&snap, asset, quote)
		return snap, sources, true, nil
	}
	if !errors.Is(err, ErrPriceNotFound) {
		return PriceSnapshot{}, nil, false, err
	}
	// Redis VWAP cache. For aggregator-rewritten pairs (XLM/fiat:USD synthesised from
	// XLM/USDC-GA5Z...) the literal pair is absent from prices_1m, so the miss above is expected and
	// the Redis vwap: key is the source of truth. Mirrors /v1/price's tryRedisVWAPFallback; the
	// provenance marker is dropped since the tip envelope has no triangulated flag.
	if cacheSnap, cacheSources, _, tri, ok := s.tryRedisVWAPFallback(ctx, asset, quote); ok {
		cacheSnap.ProxyDeviation = tri && s.proxyDeviation(ctx, time.Now().UTC())
		return cacheSnap, cacheSources, true, nil
	}
	// Read-time stablecoin-fiat proxy: rewrites X/fiat:USD to X/<peg> using the operator's
	// [trades].usd_pegged_classic_assets allow-list, as priceFallback does. Nothing on-chain quotes
	// in fiat:USD, so without it the tip would 404 on every fresh deployment.
	proxySnap, proxySources, proxyOK, proxyWithheld := s.tryStablecoinFiatProxy(ctx, asset, quote)
	if proxyOK {
		proxySnap.ProxyDeviation = s.proxyDeviation(ctx, time.Now().UTC())
		return proxySnap, proxySources, true, nil
	}
	// Fiat-vs-fiat cross-rate via the forex snapshot (see tryFiatCrossRate); no on-chain pair
	// carries fiat-vs-fiat trades.
	if fxSnap, fxSources, ok := s.tryFiatCrossRate(asset, quote); ok {
		return fxSnap, fxSources, true, nil
	}
	// USD-anchored cross for a non-fiat asset in a fiat with no market (ADR-0051, the
	// local-currency path), so the tip answers as /v1/price does instead of a 404 only this surface
	// returns. Last, so an observed market still wins.
	if fxSnap, fxSources, ok, withheld := s.tryUSDAnchoredFiatCross(ctx, asset, quote); ok {
		return fxSnap, fxSources, true, nil
	} else if withheld || proxyWithheld != "" {
		// A withheld USD leg withholds the derived price too. The peg walk carries its own reason
		// (this asset's thin market is `substance`); the cross's leg is upstream of the pair asked for.
		reason := proxyWithheld
		if reason == "" {
			reason = PriceWithheldUpstreamLeg
		}
		return PriceSnapshot{}, nil, false, newPriceWithheld(reason)
	}
	return PriceSnapshot{}, nil, false, nil
}

// parseTipAssetQuote reads asset (required) and quote (default fiat:USD), writing a 400 on failure; not shared with handlePrice, whose
// error type URLs are price-specific.
func (s *Server) parseTipAssetQuote(w http.ResponseWriter, r *http.Request) (canonical.Asset, canonical.Asset, bool) {
	rawAsset := r.URL.Query().Get("asset")
	if rawAsset == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-asset",
			"Missing asset parameter", http.StatusBadRequest,
			"asset query parameter is required")
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

// parseTipWindowSeconds reads window_seconds (default defaultTipWindowSeconds), rejecting values outside [minTipWindowSeconds,
// maxTipWindowSeconds] with a 400.
func parseTipWindowSeconds(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("window_seconds")
	if raw == "" {
		return defaultTipWindowSeconds, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < minTipWindowSeconds || n > maxTipWindowSeconds {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-window",
			"Invalid window_seconds", http.StatusBadRequest,
			"window_seconds must be an integer in [1, 60]")
		return 0, false
	}
	return n, true
}

// tipWindowEscalating runs the window VWAP at the caller's window and, when empty, once more at the 30s SLA bound; window_seconds reports
// the window used.
func (s *Server) tipWindowEscalating(ctx context.Context, asset, quote canonical.Asset, windowSeconds int, pairs []canonical.Pair) (PriceSnapshot, []string, bool) {
	if len(pairs) == 0 {
		return PriceSnapshot{}, nil, false
	}
	if snap, sources, ok := s.tipWindowVWAP(ctx, asset, quote, windowSeconds, pairs); ok {
		return snap, sources, true
	}
	if windowSeconds < tipEscalationWindowSeconds {
		return s.tipWindowVWAP(ctx, asset, quote, tipEscalationWindowSeconds, pairs)
	}
	return PriceSnapshot{}, nil, false
}

// tipWindowVWAP runs the rolling-window VWAP and returns ok=false when the window is empty or VWAP failed. Errors are swallowed on
// purpose: the tip contract guarantees
// a response if the pair has any observation, so a transient hypertable error must not 5xx while the LatestPrice fallback is healthy.
func (s *Server) tipWindowVWAP(ctx context.Context, asset, quote canonical.Asset, windowSeconds int, pairs []canonical.Pair) (PriceSnapshot, []string, bool) {
	if s.History == nil || len(pairs) == 0 {
		return PriceSnapshot{}, nil, false
	}
	now := time.Now().UTC()
	from := now.Add(-time.Duration(windowSeconds) * time.Second)

	// XLM dual-form: CEX trades live under `crypto:XLM`, on-chain under `native`, so a single-pair
	// read sees one slice and ?asset=native would fall to the stale closed-bucket fallback. MERGE the
	// alias pairs' (disjoint) trades so the tip covers all venues whichever spelling was used. Which
	// combinations merge, and why an unnamed SAC form is read last, is [tipMergePairs].
	var trades []canonical.Trade
	for _, pair := range pairs {
		tr, err := s.History.TradesInRange(ctx, pair, from, now, tipWindowMaxTrades)
		if err != nil {
			// No log under a cancelled ctx: that is the client leaving or a stream tick ending.
			if ctx.Err() == nil {
				s.logger.Warn("TradesInRange failed (tip window) — falling back to LatestPrice",
					"err", err, "asset", pair.Base.String(), "quote", pair.Quote.String(),
					"window_seconds", windowSeconds)
			}
			return PriceSnapshot{}, nil, false
		}
		trades = append(trades, tr...)
	}
	if len(trades) == 0 {
		return PriceSnapshot{}, nil, false
	}

	// Scale-normalize before VWAP: the merge mixes on-chain (7dp) and CEX (8dp) venues, so a raw
	// Σquote/Σbase mean would weight each trade by its smallest-unit magnitude and over-weight the
	// finer-scaled venue ~10x per decimal. Lift every trade to the common scale first.
	price, err := aggregate.VWAP(aggregate.NormalizeAmountScale(trades, amountScaleDecimalsFor))
	if err != nil {
		// All-zero-volume input. The fallback path will produce a
		// usable response.
		return PriceSnapshot{}, nil, false
	}

	// Forward-normalize dex-nonstandard decimals: the tip and its stream (which shares this
	// function) have no decline guard, so scaling here keeps a confirmed non-7-decimals asset from
	// being served skewed. Safe because the compute is query-time-only; a no-op otherwise.
	price = aggregate.AdjustPrice(price,
		aggregate.ResolveDecimals(s.NonstandardDecimals, asset),
		aggregate.ResolveDecimals(s.NonstandardDecimals, quote))

	sources := distinctTradeSources(trades)
	return PriceSnapshot{
		AssetID:       asset.String(),
		Quote:         quote.String(),
		Price:         ratToDecimal(price, ohlcPriceDigits),
		PriceType:     "vwap",
		ObservedAt:    WireTime(now),
		WindowSeconds: windowSeconds,
	}, sources, true
}

// tipMergePairs splits the alias-pair combinations into those the tip window MERGES and those read LAST, after every other read has
// missed (XLM aliases on both sides).
// A SAC-wrapped classic's SAC/SAC pool is far thinner than its SDEX book, so merging it unasked would let one tiny-pool trade be the tip:
// an unnamed
// SAC-form combination goes in `last`; a caller naming a SAC form keeps the full cross in `merge`. Identity combinations are dropped.
func tipMergePairs(asset, quote canonical.Asset) (merge, last []canonical.Pair) {
	sacNamed := asset.Type == canonical.AssetSoroban || quote.Type == canonical.AssetSoroban
	for _, a := range assetAliases(asset) {
		for _, q := range assetAliases(quote) {
			pair, err := canonical.NewPair(a, q)
			if err != nil {
				continue
			}
			if !sacNamed && (a.Type == canonical.AssetSoroban || q.Type == canonical.AssetSoroban) {
				last = append(last, pair)
				continue
			}
			merge = append(merge, pair)
		}
	}
	// Each set is merged into one VWAP population and the store serves a market either way round,
	// so a pair and its flip are one read; asking for both double-counts every trade (see
	// [distinctMarkets]).
	return distinctMarkets(merge), distinctMarkets(last)
}

// distinctTradeSources returns the unique source names of trades in first-occurrence order, for the
// envelope's sources array on the tip-window path.
func distinctTradeSources(trades []canonical.Trade) []string {
	if len(trades) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(trades))
	out := make([]string, 0, len(trades))
	for i := range trades {
		src := trades[i].Source
		if _, dup := seen[src]; dup {
			continue
		}
		seen[src] = struct{}{}
		out = append(out, src)
	}
	return out
}
