// Package v1 is the HTTP serving plane for the Stellar Index public
// API v1.
//
// # Source of truth
//
// The OpenAPI specification at openapi/stellar-index.v1.yaml is the
// wire contract. This package implements it. If the two diverge,
// either (a) update the spec + regenerate docs, or (b) fix the
// handler. Never silently ship a handler that disagrees with the
// spec.
//
// # Response envelope
//
// Every 2xx JSON response carries the same envelope (see [Envelope]):
//
//	{
//	  "data":       {...},
//	  "as_of":      "2026-04-22T14:30:15.842Z",
//	  "sources":    ["soroswap", "aquarius"],
//	  "flags":      {...},
//	  "pagination": {"next": "..."}   // optional
//	}
//
// Clients never have to branch on "is this key present?" — the
// envelope fields are always there (barring pagination, which only
// appears on list endpoints).
//
// # Errors
//
// Every 4xx/5xx is RFC 9457 problem+json. See [Problem].
//
// # Current-price surfaces and their windows
//
// Four surfaces serve a "current USD price" and they are NOT required
// to agree to the last digit. Each is pinned to a different window by
// design (ADR-0018 makes the consistency contract a property of the
// URL, not of a query parameter), so a live pair that moves 0.1 %/min
// routinely shows a ~0.1–0.2 % spread between them. That spread is the
// windows differing, not an aggregation bug — B10-F1 (audit-2026-07)
// measured ~0.16 % and resolved to "document the semantics":
//
//	/v1/price          Last CLOSED 1m VWAP bucket (ADR-0015). Cross-region
//	                   byte-identical for the same (pair, window, from_ts);
//	                   worst-case ~30–120 s stale by construction. Reads
//	                   PriceReader.LatestPrice (aggregator's Redis
//	                   price:<pair> hot key, prices_1m fallback), with the
//	                   asset-alias loop (native ↔ crypto:XLM), the
//	                   stablecoin-fiat proxy (ADR-0026) and the fiat
//	                   cross-rate synthesis behind it. `?window=N` switches
//	                   to the aggregator's continuously-published rolling
//	                   VWAP for that window — still not a tip read.
//	/v1/price/tip      Rolling [now-5s, now) VWAP, escalating to 30 s when
//	                   the short window is empty, then falling back to the
//	                   same LatestPrice snapshot. No cross-region guarantee.
//	                   See [Server.handlePriceTip].
//	/v1/observations   Raw most-recent trade PER SOURCE. No aggregation at
//	                   all, so a single venue's print — not comparable to a
//	                   VWAP by construction.
//	/v1/assets…        price_usd inlined on the listing + detail rows. Two
//	                   producers, in precedence order: (1) the asset-
//	                   catalogue SQL overlay — the most recent prices_1m
//	                   bucket in a trailing 24 h window, quoted through the
//	                   on-chain USDC/fiat:USD proxy pair and chained via
//	                   XLM for assets with no direct USD market, served
//	                   through CachedAssetsReader (2 min TTL, stale-while-
//	                   revalidate in the API binary); (2) when the overlay
//	                   has no row, [Server.lookupUSDPrice] — the SAME
//	                   closed-bucket PriceReader read /v1/price does.
//	                   Catalogue (verified-currency) rows instead price
//	                   through aggregate.ComputeGlobalPrice's three CEX/
//	                   aggregator tiers, and fiat rows through fx_quotes'
//	                   DAILY ECB reference rate — a fiat price_usd can
//	                   legitimately be up to ~24 h old.
//
// Consequences worth stating plainly, because consumers hit them:
//
//   - Only /v1/price carries the cross-region determinism guarantee.
//     Anything reconciling two regions, or auditing a historical claim,
//     must read /v1/price (or the historical surfaces) — never the
//     asset rows, whose 2-minute response cache alone can put two
//     regions on different buckets.
//   - `flags.stale` means "below THIS surface's baseline contract", so
//     it is not comparable across surfaces: /v1/price sets it when it
//     degraded off the closed bucket, /v1/price/tip never sets it (both
//     of its branches are in-contract).
//   - The historical surfaces (/v1/history, /v1/ohlc, /v1/twap,
//     /v1/vwap, /v1/chart, /v1/price/at) are bucket-pinned to an
//     explicit [from, to) and clamp an implicit "now" to the
//     closedBucketWindow boundary, so they are reproducible and are not
//     part of this spread.
//
// # Middleware stack
//
// Applied in order (outermost first). See [Server.Handler] for the
// authoritative ordering + rationale on each placement:
//
//	RequestID        → assigns X-Request-ID if absent / safe.
//	HTTPMetrics      → records http_requests_total + duration hist.
//	Logger           → structured access log per request (slog);
//	                   populates remote_ip into ctx for downstream.
//	Recoverer        → handler panics → 500 problem+json.
//	SecurityHeaders  → X-Content-Type-Options: nosniff on every resp.
//	CORS (optional)  → allow-list from [config.APIConfig.AllowedOrigins];
//	                   outside RateLimit so OPTIONS preflight is free.
//	RateLimit (opt.) → per-IP token bucket (internal/ratelimit); innermost
//	                   so it sees Logger-populated remote_ip.
//
// # What this package doesn't do
//
//   - No auth logic in the handlers themselves — the
//     [middleware/auth.go] `Auth` middleware identifies the
//     subject (apikey or sep10), stamps it on the request
//     context, and the handlers consume it via
//     `auth.SubjectFrom(ctx)`. Validators live in
//     [internal/auth] (the [auth.APIKeyValidator] /
//     [auth.SEP10Validator] interfaces) with concrete impls in
//     [internal/auth/sep10.Validator] and the Redis-backed
//     [auth.RedisAPIKeyValidator].
//   - No serialisation of canonical types — they handle themselves
//     via their [encoding/json.Marshaler] implementations.
//   - No business logic — that lives in [internal/aggregate] and
//     [internal/storage/timescale].
//
// The handlers are thin: parse input → call a storage or cache
// method → wrap in an Envelope → write JSON.
//
// # References
//
//   - [docs/reference/api-design.md] — design doc.
//   - [openapi/stellar-index.v1.yaml] — wire contract.
//   - [ADR-0007] — Redis cache schema (cachekeys).
package v1
