package v1

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Tip-surface tunables per ADR-0018.
//
// defaultTipWindowSeconds matches the ADR's default; minTipWindowSeconds
// and maxTipWindowSeconds enforce the documented clamp. The cap exists
// to keep the rolling-window scan cheap even when the underlying
// hypertable has millions of rows in a 60s span on a hot pair — and to
// keep the surface honest about being a "tip", not a small-window
// historical aggregator (that's /v1/vwap's job).
const (
	defaultTipWindowSeconds = 5
	minTipWindowSeconds     = 1
	maxTipWindowSeconds     = 60

	// tipWindowMaxTrades caps the scan within a tip window. The rolling
	// window is short (≤60s), so this is the worst-case row count we
	// load into memory for one VWAP. Mirrors /v1/vwap's own cap.
	tipWindowMaxTrades = 10000

	// tipEscalationWindowSeconds is the widened retry window when the
	// caller's (or default 5s) window contains no trades. Board #42
	// (RFP audit): the previous behavior fell STRAIGHT from an empty
	// 5s window to the closed-bucket store price (60–113s stale) —
	// live samples showed ~90s staleness on /v1/price/tip whenever a
	// quiet second was hit, breaching the ≤30s freshness SLA the tip
	// surface exists to serve. Escalating to a 30s window first means
	// staleness exceeds 30s ONLY when the pair genuinely had no trade
	// in the last 30s (at which point the closed bucket is the honest
	// answer and observed_at says so). 30 = the SLA bound, hence not
	// configurable.
	tipEscalationWindowSeconds = 30
)

// handlePriceTip serves GET /v1/price/tip per ADR-0018.
//
// Two in-contract response branches:
//
//   - Window VWAP: at least one trade in [now-window_seconds, now). The
//     handler returns the VWAP with price_type="vwap", window_seconds=N.
//   - Last-good fallback: window is empty. The handler returns
//     PriceReader.LatestPrice's most-recent observation as-is — no
//     synthetic age cap, the customer reads observed_at and decides.
//
// flags.stale is **always false** on this surface — both branches are
// in-contract per ADR-0018 §"flags.stale semantic". The freeze flag
// also stays unset here: freeze is a closed-bucket concept and the tip
// surface has no closed-bucket guarantee. Divergence flagging still
// applies (asset-level, not bucket-level).
//
// URL discipline: ?granularity= is rejected with 400 — granularity is
// a closed-bucket concept and accepting it on the tip URL would let a
// stray query string silently turn a tip request into something
// closed-bucket-shaped (ADR-0018 §"URL discipline").
//
// Window semantics vs the other price surfaces: a 5 s (default) rolling
// window against /v1/price's last CLOSED 1 m bucket and against the
// trailing-24 h catalogue overlay behind /v1/assets' price_usd. On a
// moving pair the three legitimately differ by ~0.1–0.2 % — see the
// "Current-price surfaces and their windows" section in the package
// doc, which enumerates every producer and its window.
func (s *Server) handlePriceTip(w http.ResponseWriter, r *http.Request) {
	// PriceReader is the fallback path; without it the tip surface
	// can't degrade and there's nothing meaningful to serve. The
	// rolling-window path needs HistoryReader but we'll degrade
	// gracefully when only one of them is wired.
	if s.prices == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Price serving not configured", http.StatusServiceUnavailable,
			"this deployment has no PriceReader wired — check binary configuration")
		return
	}

	// Reject URL-discipline violations BEFORE asset/quote parsing —
	// a request that mixes tip + closed-bucket semantics is malformed
	// regardless of whether the asset/quote happen to parse.
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
		writePriceWithheldProblem(w, r, asset, quote)
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

	writeJSON(w, snapshot, s.tipFlags(r.Context(), asset, sources), sources...)
}

// tipFlags builds the envelope flags for one tip emission. The request
// endpoint and both stream producers (per-connection and Hub-shared)
// share it, so a tip_update event carries exactly the flags the GET
// would have served for the same computation — the stream is documented
// as "same compute logic", and the flags are part of that.
//
// Per ADR-0018: stale stays FALSE on either branch — both are
// in-contract on this surface. The staleness bit PriceReader sets for
// /v1/price is deliberately ignored; tip has its own envelope contract.
// divergence_warning/divergence_checked come from the shared
// alias-walking lookup, keyed on the base like every other surface that
// carries them.
func (s *Server) tipFlags(ctx context.Context, asset canonical.Asset, sources []string) Flags {
	flags := Flags{SingleSource: len(sources) == 1}
	flags.DivergenceWarning, flags.DivergenceChecked = s.lookupDivergenceFlag(ctx, asset)
	return flags
}

// computeTip is the shared core of [Server.handlePriceTip] and
// [Server.handlePriceTipStream]. Tries the rolling-window VWAP first
// (when HistoryReader is wired and the window has trades), falling
// back to PriceReader.LatestPrice when the window is empty, then to
// the aggregator's Redis VWAP cache for stablecoin-fiat-proxy
// rewritten pairs whose literal form is absent from prices_1m.
//
// Returns ErrPriceNotFound when no branch can produce a snapshot —
// caller turns that into 404 on the request endpoint and into
// "stream cannot start" on the stream endpoint. Any other error is
// surfaced as-is for caller-side logging + 500 mapping.
func (s *Server) computeTip(ctx context.Context, asset, quote canonical.Asset, windowSeconds int) (PriceSnapshot, []string, error) {
	// Thin-market substance gate, checked FIRST: the tip surface
	// promises freshness, not provability (ADR-0018), but it is still
	// an aggregated "the price of X is P" claim — and the rolling-
	// window VWAP below is computed straight from raw trades, so
	// without this check a dust-authored market would serve its
	// attacker-written rate here even after /v1/price started
	// withholding it (the 2026-08-04 incident class). One gate call
	// covers every branch of this function; the reader-level gates
	// inside LatestPrice would otherwise cover only the middle one.
	//
	// Deliberately NOT given a best-effort sub-budget the way the
	// event's divergence lookup is (see [tipStreamDivergenceBudget]).
	// That treatment is only available to a signal with a safe unknown
	// to degrade to: the divergence flag has one, because
	// `divergence_checked: false` already means "could not verify". A
	// withholding gate has none — it decides whether to serve AT ALL,
	// and the only two things a timed-out gate could do are refuse a
	// price that is fine or publish one the gate exists to withhold.
	// Timing this out on the emit path would republish an
	// attacker-authored rate the 2026-08-04 incident class made these
	// gates for, so a slow gate correctly costs the emission instead.
	if s.substance != nil && !s.substance.Allowed(ctx, asset, quote, "tip") {
		return PriceSnapshot{}, nil, ErrPriceWithheld
	}
	// Scam-issuer gate: same posture as the substance gate on this
	// surface — a directory-scam-flagged issuer's live tip is still an
	// aggregated price claim we decline to publish. Keyed on the base, so
	// it covers every quote (including the XLM-triangulated headline).
	if s.scam != nil && s.scam.Withheld(ctx, asset, "tip") {
		return PriceSnapshot{}, nil, ErrPriceWithheld
	}
	// Which alias combinations the window merges, and which it holds back
	// until every other read has missed, is [tipMergePairs].
	merge, last := tipMergePairs(asset, quote)
	if snap, sources, ok := s.tipWindowEscalating(ctx, asset, quote, windowSeconds, merge); ok {
		return snap, sources, nil
	}
	// Fallback: most-recent known observation for the pair. PriceReader
	// returns price_type="last_trade" today (MVP) and "vwap" once the
	// aggregator wires the closed-bucket cache; both are in-contract
	// for the tip fallback per ADR-0018 (the customer reads price_type
	// + observed_at to know what they got).
	//
	// F-1340: route through the rc.89 XLM dual-form alias loop, exactly
	// as handlePrice does, so /v1/price/tip?asset=native resolves a
	// fresh crypto:XLM observation rather than missing it on the
	// literal form.
	snap, sources, _, err := s.readPriceWithAliases(ctx, s.prices, asset, quote)
	if err == nil {
		// dex-nonstandard-decimals forward normalization (M2): this
		// closed-bucket / last-trade fallback returns the RAW asset/quote ratio,
		// exactly like /v1/price's readPriceWithAliases read. The tip window
		// VWAP path (tipWindowVWAP) already normalizes; this fallback branch did
		// NOT, so a confirmed non-7-decimals asset served a skewed tip here.
		// Byte-identical no-op at 7dp. (The Redis/proxy/fiat branches below
		// self-normalize at their own source — see tryStablecoinFiatProxy.)
		s.normalizeRawPriceSnapshot(&snap, asset, quote)
		return snap, sources, nil
	}
	if !errors.Is(err, ErrPriceNotFound) {
		return PriceSnapshot{}, nil, err
	}
	// Final fallback: Redis VWAP cache. For aggregator-rewritten pairs
	// (XLM/fiat:USD synthesised from XLM/USDC-GA5Z…) the literal pair
	// is absent from prices_1m so the storePriceReader miss above is
	// expected. The Redis vwap: key IS the source of truth for these
	// values. Mirrors the /v1/price handler's tryRedisVWAPFallback so
	// both surfaces serve the same data; provenance marker (when
	// present) is dropped since the tip envelope has no triangulated
	// flag — operators reading the marker for forensics use /v1/price
	// instead.
	if cacheSnap, cacheSources, _, ok := s.tryRedisVWAPFallback(ctx, asset, quote); ok {
		return cacheSnap, cacheSources, nil
	}
	// Read-time stablecoin-fiat proxy: rewrites X/fiat:USD to X/<peg>
	// at request time using the operator's
	// [trades].usd_pegged_classic_assets allow-list. Mirrors the
	// equivalent fallback in priceFallback (#1217). Without this
	// /v1/price/tip?asset=native&quote=fiat:USD 404s out of the box on
	// every fresh deployment because nothing on-chain ever quotes in
	// fiat:USD — same exact failure mode as /v1/price had.
	if proxySnap, proxySources, ok, _ := s.tryStablecoinFiatProxy(ctx, asset, quote); ok {
		return proxySnap, proxySources, nil
	}
	// Last-resort fiat-vs-fiat cross-rate via the forex snapshot.
	// Same machinery /v1/price uses (see tryFiatCrossRate). Without
	// this branch /v1/price/tip?asset=fiat:EUR&quote=fiat:USD 404s
	// because no on-chain pair carries fiat-vs-fiat trades.
	if fxSnap, fxSources, ok := s.tryFiatCrossRate(asset, quote); ok {
		return fxSnap, fxSources, nil
	}
	// USD-anchored cross for a non-fiat asset in a fiat we have no
	// market for (ADR-0051) — the local-currency path. Present here for
	// the same reason the two branches above are: a wallet polling the
	// tip for a BRL-denominated balance must get the same answer
	// /v1/price gives it, not a 404 that only this surface returns.
	// Last, so an observed market still wins.
	if fxSnap, fxSources, ok, withheld := s.tryUSDAnchoredFiatCross(ctx, asset, quote); ok {
		return fxSnap, fxSources, nil
	} else if withheld {
		// The USD leg is withheld, so the derived price is too. This
		// surface already distinguishes the two verdicts (see the
		// ErrPriceWithheld arm above), so report it honestly rather than
		// letting it fall through as "no data".
		return PriceSnapshot{}, nil, ErrPriceWithheld
	}
	// Last of all: the SAC-form combinations the caller did not name —
	// for a wrapped classic, its Soroban SAC/SAC pool. Every established
	// read above has missed (the window at the caller's bound and at
	// 30s, the closed bucket, the caches, the proxies), so the
	// alternative is no price at all; this is the alias family's
	// SAC-last shape (canonical.AssetAliases) applied to the one walk
	// that merges. A Soroban-only wrapped classic serves from its pool
	// here; a classic with any established answer never reaches it.
	if snap, sources, ok := s.tipWindowEscalating(ctx, asset, quote, windowSeconds, last); ok {
		return snap, sources, nil
	}
	return PriceSnapshot{}, nil, ErrPriceNotFound
}

// parseTipAssetQuote pulls asset (required) + quote (defaulted to
// fiat:USD) from the request, writes a 400 + returns ok=false on any
// validation failure. Mirrors the equivalent parsing in handlePrice
// rather than sharing a helper — handlePrice writes its own
// price-specific error type URLs and we want the tip handler's
// errors to be self-explanatory in problem+json.
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

// parseTipWindowSeconds reads the optional window_seconds query param,
// defaulting to defaultTipWindowSeconds and rejecting values outside
// [minTipWindowSeconds, maxTipWindowSeconds]. Returns (seconds, true)
// on success or (0, false) after writing a 400.
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

// tipWindowEscalating runs the rolling-window VWAP over pairs at the
// caller's window and, when that window is empty, once more at the 30s
// SLA bound (board #42) before the caller drops to its next fallback.
// The response's window_seconds reports the window actually used. An
// empty pair set is simply a miss.
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

// tipWindowVWAP runs the rolling-window VWAP path. Returns
// (snapshot, sources, true) when at least one trade landed in the
// window and VWAP succeeded; (_, _, false) otherwise so the caller
// drops to the LatestPrice fallback.
//
// Errors are intentionally swallowed (logged when the request
// context is still alive, not surfaced) — the tip contract
// guarantees the caller a response if the pair has any observation
// at all, and the LatestPrice fallback is the authoritative answer
// when the rolling window can't produce one. Surfacing a 5xx here
// would make a transient hypertable hiccup turn the entire tip
// surface red even when the fallback is healthy.
func (s *Server) tipWindowVWAP(ctx context.Context, asset, quote canonical.Asset, windowSeconds int, pairs []canonical.Pair) (PriceSnapshot, []string, bool) {
	if s.history == nil || len(pairs) == 0 {
		return PriceSnapshot{}, nil, false
	}
	now := time.Now().UTC()
	from := now.Add(-time.Duration(windowSeconds) * time.Second)

	// XLM dual-form (rc.89 / F-1340): trades for the SAME asset live under
	// different canonical ids per source class — CEX trades under
	// `crypto:XLM`, on-chain under `native`. A single-pair read sees only
	// one slice, which made ?asset=native fall through to the closed-bucket
	// fallback (61–113s stale) while ?asset=crypto:XLM was fresh — failing
	// the ≤30s freshness contract for the natural spelling. MERGE the
	// alias pairs' trades (disjoint sets) so the tip VWAP covers all venues
	// regardless of which spelling the caller used. Which combinations
	// are merged — and why an unnamed SAC-form one is read only after
	// every other read has missed — is [tipMergePairs]; the caller
	// passes whichever set this read is for.
	var trades []canonical.Trade
	for _, pair := range pairs {
		tr, err := s.history.TradesInRange(ctx, pair, from, now, tipWindowMaxTrades)
		if err != nil {
			// Don't log under a cancelled ctx — that's just the client
			// disconnecting (or, on the stream path, the per-tick scope
			// completing).
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

	// Scale-normalize before VWAP: the alias loop above deliberately MERGES
	// on-chain (native, 7dp) and CEX (crypto:XLM, 8dp) venues into one slice,
	// so — exactly as on the fiat-combine point path — the raw Σquote/Σbase
	// mean would weight each trade by its smallest-unit magnitude
	// (real_volume × 10^scale) and over-weight the finer-scaled venue ~10×
	// per decimal. Lift every trade to the common scale first. A single-venue
	// window (the common case) is byte-identical (CS-040).
	trades = aggregate.NormalizeAmountScale(trades, amountScaleDecimalsFor)

	price, err := aggregate.VWAP(trades)
	if err != nil {
		// All-zero-volume input. The fallback path will produce a
		// usable response.
		return PriceSnapshot{}, nil, false
	}

	// dex-nonstandard-decimals forward normalization. /v1/price/tip (and
	// its SSE sibling /v1/price/tip/stream, which shares this function)
	// had NO decline guard at all — declineIfNonstandardDecimals's
	// original four-endpoint list omitted the tip surface, so a confirmed
	// non-7-decimals asset's skewed price was reaching customers here
	// live and unguarded even after the 2026-07-09 guard shipped. Scaling
	// the ratio below closes that gap directly rather than adding a
	// decline (the compute is query-time-only here, same as VWAP/TWAP/
	// OHLC single-bar, so normalizing is safe). No-op for any pair with
	// no confirmed non-7-decimals leg.
	price = aggregate.AdjustPrice(price,
		aggregate.ResolveDecimals(s.nonstandardDecimals, asset),
		aggregate.ResolveDecimals(s.nonstandardDecimals, quote))

	sources := distinctTradeSources(trades)
	return PriceSnapshot{
		AssetID:       asset.String(),
		Quote:         quote.String(),
		Price:         ratToDecimal(price, ohlcPriceDigits),
		PriceType:     "vwap",
		ObservedAt:    now,
		WindowSeconds: windowSeconds,
	}, sources, true
}

// tipMergePairs partitions the alias-pair combinations of a requested
// (asset, quote) into the set the tip window MERGES and the set it reads
// LAST — only after every other read has missed.
//
// Both sides alias, because XLM appears as a quote too (AQUA/XLM), and
// the merge is what lets ?asset=native reach the CEX prints stored under
// crypto:XLM — the two established XLM forms are deep, disjoint venue
// populations of one asset.
//
// Every SAC-wrapped classic has the same dual identity — the families the
// operator declares in `[supply].sac_wrappers` — and its Soroban SAC/SAC
// pool is routinely orders of magnitude thinner than its SDEX book. A
// merge that admitted the pool unasked let ONE trade on a
// few-hundred-dollar pool be the served tip of a classic-quoted request
// in every 30s window the SDEX book was silent, with no gate on the pool
// itself: the substance gate measures the alias union, which the deep
// book clears on the pool's behalf, the trailing-baseline guard never
// runs on this surface, and the window VWAP is computed straight from
// raw trades. That is the thin-pool third-alias shape the alias family's
// SAC-LAST ordering exists to stop (see canonical.AssetAliases); a merge
// has no "last", so this gives it one. A SAC-form combination the caller
// did not name is never merged: it is returned in `last`, and computeTip
// reads it only after the established combinations' window, the
// closed-bucket read and every other fallback have missed — where the
// alternative is no price at all. A wrapped classic with no classic
// venue (a Soroban-only market) therefore still serves from its pool —
// gated by the substance floor, which for such an asset measures the
// pool alone — and a SAC print can never displace, or blend into, an
// answer the established forms can give.
//
// A caller who names a SAC form is asking about that market and keeps
// the full cross — the pool merged with its classic sibling — in
// `merge`, with nothing held back. Identity combinations
// (native/crypto:XLM collapsing) are dropped.
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
	// Each set is MERGED into one VWAP population, and the store now
	// serves a market whichever way round it is asked for, so a pair and
	// its flip are one read. Asking for both double-counts every trade
	// and sums two orientations into one mean — see [distinctMarkets].
	return distinctMarkets(merge), distinctMarkets(last)
}

// distinctTradeSources returns the unique source names from a slice
// of trades, preserving first-occurrence order. Used to populate the
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
