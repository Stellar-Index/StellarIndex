package middleware

import (
	"net/http"
	"regexp"
	"strings"
)

// CacheControl is a middleware that sets the Cache-Control response
// header per the route's caching policy. CDN tier (e.g. CloudFront)
// reads `s-maxage`; client tier reads `max-age`. The two-tier setup
// lets a hot path absorb a 100× burst at the CDN without filling
// the origin budget while still serving fresh-enough data to clients.
//
// Policy (per ADR-0018 surface model):
//
//   - **Health / version / metrics** → `no-store` (operator endpoints
//     change every probe; caching them would mask outages).
//   - **Account endpoints** → `private, no-store` (auth-tied; never
//     caches across users; CDN MUST NOT see them).
//   - **Tip / observations / diagnostics** → `private, no-cache,
//     must-revalidate` (tip surface intentionally has no cross-region
//     consistency contract per ADR-0018; caching shifts the contract.
//     `/v1/diagnostics/*` is operator-facing live data — the
//     explorer polls it every 15 s, so caching defeats the UX).
//   - **Closed-bucket historical + catalogues** (`/v1/history*`,
//     `/v1/ohlc`, `/v1/vwap`, `/v1/twap`, `/v1/markets`, `/v1/pairs`,
//     `/v1/oracle/*`, `/v1/sources`, `/v1/assets*`, `/v1/issuers*`,
//     `/v1/changes/*`) → `public, max-age=60, s-maxage=300` (1 min
//     client / 5 min CDN). Closed buckets are immutable per
//     ADR-0015, but the trailing-edge boundary advances as time
//     passes — the s-maxage caps how long a CDN entry stays
//     before the boundary moves.
//   - **Current price + asset detail** → `public, max-age=30,
//     s-maxage=60` (more aggressive refresh; these update on every
//     bucket close).
//
// Handlers MAY override the middleware's directive by setting
// Cache-Control before they call writeJSON / writeProblem (the
// middleware sets the header BEFORE calling the inner handler).
// Override is the exception, not the rule — the middleware's
// directive is the right answer for >99% of requests.
//
// Errors override the route's directive at the writer side. ALL
// problem+json writers (writeProblem in v1/envelope.go, the rate
// limiter's writeRateLimitProblem, the recoverer's panic body, the
// envelope404 middleware that rewrites the mux's text/plain 404/405,
// writeAuthProblem, writeKeyPolicyDenied, writeEmailUnverified, and
// the monthly-quota writer) explicitly set `Cache-Control: no-store`
// before WriteHeader — a new problem writer MUST do the same. Without that override an error response would inherit
// (e.g.) `public, max-age=60, s-maxage=300` from the catalogue
// surface and a CDN would happily cache the transient failure for
// 5 minutes against the same key as the success response.
//
// Backwards-compat shim: behaves like cdn_enabled=true. Operators
// who run the API behind no CDN should use [CacheControlWithCDN]
// to drop the `s-maxage` half of the directive.
func CacheControl(next http.Handler) http.Handler {
	return CacheControlWithCDN(true)(next)
}

// CacheControlWithCDN returns the cache-control middleware with the
// `s-maxage` (CDN-tier) directive controlled by `cdnEnabled`. When
// false, only `max-age` (client tier) is emitted on cacheable
// routes — appropriate for deployments without a CDN in front.
// `private, no-store` and `private, no-cache, must-revalidate`
// directives are unaffected (they were never CDN-cacheable).
func CacheControlWithCDN(cdnEnabled bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", policyForPath(r.URL.Path, cdnEnabled))
			next.ServeHTTP(w, r)
		})
	}
}

// policyForPath classifies a request path into a Cache-Control
// directive. Exposed at package scope so tests can pin the policy
// table without spinning up a full handler.
//
// Order matters — the more-specific prefix MUST win over the
// less-specific. `/v1/price/tip` is private; `/v1/price` is public —
// both share the prefix `/v1/price` so the tip rule must run first.
//
// `cdnEnabled` controls whether `s-maxage` (CDN-tier) directives
// are emitted on cacheable routes. When false, only `max-age`
// (client tier) survives — operators without a CDN in front of
// the API set this so a CDN they don't have can't cache anything.
// Closed-ledger detail paths (#332 F3, 2026-09-02). A ledger's own row, its
// transaction list and a transaction by hash are IMMUTABLE once the ledger
// has closed, yet they fell through to the conservative default and were
// served `private, no-store` — the explorer re-fetched a 71 KB transaction
// list on every visit to a ledger page. The band below is deliberately
// modest (1 min client / 5 min CDN), not a year: policyForPath knows
// nothing about the tip, and a ledger a few seconds old can be served
// before every downstream projection for it has landed, so a long TTL
// could pin a partial view. Five minutes rides out that lag and still
// absorbs the repeat-visit cost.
var (
	ledgerDetailPath = regexp.MustCompile(`^/v1/ledgers/[0-9]+(/transactions|/operations)?$`)
	txDetailPath     = regexp.MustCompile(`^/v1/tx/[0-9a-fA-F]{64}$`)
)

// ledgerPolicy classifies the operator probes and the explorer's ledger/tx
// surface (#332 F3, 2026-09-02). ok=false means "not one of mine — fall
// through to the main switch".
//
//   - A ledger's own row, its transaction/operation list and a transaction by
//     hash are IMMUTABLE once the ledger closes, yet they fell through to the
//     conservative default and were served `private, no-store` — the explorer
//     re-fetched a 71 KB transaction list on every visit to a ledger page. The
//     band is deliberately modest (1 min client / 5 min CDN), not a year:
//     policyForPath knows nothing about the tip, and a ledger a few seconds
//     old can be served before every downstream projection for it has
//     landed, so a long TTL could pin a partial view.
//   - /v1/ledgers (the list) moves every ~5 s and /v1/network/throughput
//     already has a server-side cache; both get the status-like short band.
func ledgerPolicy(path string, cdnEnabled bool) (string, bool) {
	switch {
	// Operator endpoints — probed by systemd/Prometheus/uptime checks; a
	// cached probe is a lie. (Moved here from the main switch with the
	// ledger cases so policyForPath stays under the gocyclo ceiling.)
	case path == "/v1/healthz", path == "/v1/readyz", path == "/v1/version", path == "/metrics":
		return "no-store", true
	case ledgerDetailPath.MatchString(path), txDetailPath.MatchString(path):
		if cdnEnabled {
			return "public, max-age=60, s-maxage=300", true
		}
		return "public, max-age=60", true
	case path == "/v1/ledgers", path == "/v1/network/throughput":
		if cdnEnabled {
			return "public, max-age=10, s-maxage=15", true
		}
		return "public, max-age=10", true
	}
	return "", false
}

func policyForPath(path string, cdnEnabled bool) string {
	// Closed-ledger detail + the two fast-moving explorer reads (#332 F3)
	// live in their own helper so this switch stays under the gocyclo
	// ceiling; see ledgerPolicy for the rationale on each band.
	if p, ok := ledgerPolicy(path, cdnEnabled); ok {
		return p
	}
	switch {
	// ─── Operator endpoints — never cached ──────────────────────

	// ─── Account endpoints — auth-tied, MUST NOT hit CDN ────────
	case strings.HasPrefix(path, "/v1/account/"):
		return "private, no-store"

	// ─── SEP-10 Web Auth — credential exchange MUST NOT hit CDN ─
	// Caching the challenge would let a future request reuse a
	// nonce; caching the token would expose it to anyone the CDN
	// serves. Both unconditionally bypass cache.
	case strings.HasPrefix(path, "/v1/auth/sep10"):
		return "private, no-store"

	// ─── Magic-link auth + dashboard — same trust class as SEP-10
	// F-1225 (audit-2026-05-12): /v1/auth/{login,callback,logout}
	// + /v1/dashboard/keys* fell through to the no-match branch
	// (no Cache-Control set), so a CDN in front of the API could
	// have cached /v1/auth/callback's session-cookie response and
	// re-issued it to subsequent requests. /v1/signup is also a
	// credential / state-changing surface that must never cache.
	// /v1/methodology
	// + /v1/incidents.atom + /v1/price/stream are not credential
	// surfaces but had no explicit policy — fold them in here so
	// no v1 route reaches the no-match default branch.
	case strings.HasPrefix(path, "/v1/auth/"),
		strings.HasPrefix(path, "/v1/dashboard/"),
		path == "/v1/signup":
		return "private, no-store"

	// ─── SSE streams — bypass CDN cache; the response is a long-
	// lived event stream, not a cacheable body. Without this CDNs
	// may try to buffer + replay.
	case strings.HasPrefix(path, "/v1/price/stream"):
		return "no-store"

	// ─── Public-but-policy-opinionated paths that lacked an
	// explicit case before F-1225. Methodology page is mostly
	// static prose; the atom feed is poll-cadence content.
	case path == "/v1/methodology":
		if cdnEnabled {
			return "public, max-age=300, s-maxage=600"
		}
		return "public, max-age=300"
	case path == "/v1/incidents.atom":
		if cdnEnabled {
			return "public, max-age=60, s-maxage=120"
		}
		return "public, max-age=60"

	// ─── Tip + observations — private surfaces (ADR-0018) ───────
	// Tip has no cross-region consistency contract; caching
	// would shift the contract. Same for observations.
	case path == "/v1/price/tip",
		strings.HasPrefix(path, "/v1/price/tip/"),
		path == "/v1/observations",
		strings.HasPrefix(path, "/v1/observations/"):
		return "private, no-cache, must-revalidate"

	// ─── Diagnostics — operator-facing live data ────────────────
	// /v1/diagnostics/cursors is polled every 15s by the explorer
	// /diagnostics page; caching would defeat the "watch the
	// indexer tick" UX. Same shape as tip/observations: tight
	// freshness, never CDN-cached.
	case strings.HasPrefix(path, "/v1/diagnostics/"):
		return "private, no-cache, must-revalidate"

	// ─── Per-source health — same freshness class as diagnostics ──
	// /v1/sources/{name}/health serves the 15s-refreshed ingestion-
	// snapshot row; the explorer source page polls it. Note the
	// trailing slash: the exact-match `/v1/sources` catalogue path in
	// the closed-bucket block below is unaffected.
	case strings.HasPrefix(path, "/v1/sources/"):
		return "private, no-cache, must-revalidate"

	// ─── Status — customer-facing health rollup ─────────────────
	// /v1/status is what the explorer /status page polls every 10 s
	// and what monitoring dashboards (and the smoke timer) poll on a
	// longer interval. A 10 s cache absorbs the polling fan-out
	// without delaying alert-state propagation enough to matter —
	// the underlying signals (Prometheus heartbeats, incident counts)
	// already have 15 s scrape granularity.
	case path == "/v1/status":
		if cdnEnabled {
			return "public, max-age=10, s-maxage=15"
		}
		return "public, max-age=10"

	// ─── Closed-bucket price surfaces — VERY short shared cache ──
	// ADR-0015/0018 is a DETERMINISM contract (byte-identical for the
	// same (pair, window, from_ts)), not a freshness bound — so a shared
	// cache serving a previous closed bucket keeps determinism while
	// breaking the "MOST RECENT closed bucket" clause, which is exactly
	// what the SLA probe measures (150 s target: 60 s bucket + 30 s CAGG
	// end_offset + <=30 s schedule + runtime).
	//
	// A shared TTL of d adds d to the worst-case age of `observed_at`,
	// makes the per-request `as_of` lie by up to d, and extends by d the
	// window in which a pre-freeze price is served with `frozen=false`
	// after a phase-2 freeze fires. The old s-maxage=60 was not provable
	// on any of the three: it can serve a bucket a full bucket behind
	// origin (age <= 210 s, past the 150 s probe bound) and stale
	// `frozen` / `confidence` for two 30 s aggregator ticks.
	//
	// 5 s is: inside the probe bound (<=155 s), inside one aggregator
	// tick, and inside one bucket for the large majority of fills.
	// `max-age` stays 30 s — private client reuse is the client's own
	// copy, not a shared-cache lie. (The only PROVABLE alternative is a
	// per-response s-maxage = secondsUntil(bucketEnd + 30 s); it needs
	// bucket phase in the middleware, which this layer does not have.)
	//
	// /v1/oracle/latest joins them: it is a "latest observation per
	// source" surface with NO closed-bucket contract and no staleness
	// flag, and it sat in the 300 s catalogue band purely because of the
	// `/v1/oracle/` prefix arm below. #344.
	case path == "/v1/price",
		strings.HasPrefix(path, "/v1/price/batch"),
		path == "/v1/price/changes",
		path == "/v1/oracle/latest":
		if cdnEnabled {
			return "public, max-age=30, s-maxage=5"
		}
		return "public, max-age=30"

	// ─── Current price + asset detail — short cache ─────────────
	// Updates on every bucket close; CDN entry should turn over
	// inside one bucket so consumers see fresh closed-bucket data.
	case path == "/v1/assets",
		strings.HasPrefix(path, "/v1/assets/"),
		// Pool reserves — CURRENT contract state from the lake; can
		// change every ledger (~5 s) but the explorer polls it, so
		// the short band absorbs fan-out while staying honest about
		// "current". Exact-match: the /v1/pools listing keeps its
		// longer closed-window cache in the catalogue band below.
		path == "/v1/pools/reserves",
		// Native (CAP-38) liquidity-pool reserves — same CURRENT-lake-
		// state nature as /v1/pools/reserves (both listing + ?pool=).
		path == "/v1/liquidity-pools",
		// SDEX order-book depth — CURRENT offer state from the
		// in-process book, which itself advances every ~60s; the
		// short band absorbs widget polling without overstating
		// freshness the snapshot doesn't have.
		path == "/v1/sdex/orderbook":
		if cdnEnabled {
			return "public, max-age=30, s-maxage=60"
		}
		return "public, max-age=30"

	// ─── Historical / closed-bucket / catalogue — longer cache ──
	// Closed buckets are immutable per ADR-0015 but the
	// trailing-edge boundary advances; s-maxage=300 caps how long
	// a CDN entry can lag the boundary.
	case strings.HasPrefix(path, "/v1/history"),
		// Point-in-time price — an immutable closed bucket keyed by a
		// fixed (asset, quote, ts); as cacheable as /v1/ohlc.
		path == "/v1/price/at",
		path == "/v1/ohlc",
		path == "/v1/vwap",
		path == "/v1/twap",
		path == "/v1/markets",
		path == "/v1/pairs",
		path == "/v1/sources",
		strings.HasPrefix(path, "/v1/oracle/"),
		// /v1/chart is closed-bucket OHLCV (ADR-0015 contract);
		// same caching semantics as /v1/ohlc / /v1/history.
		path == "/v1/chart",
		// Pools listing (DEX/AMM rows from the trades hypertable);
		// refresh cadence matches /v1/markets.
		path == "/v1/pools",
		// Lending pools — Blend pool list; same registry shape.
		path == "/v1/lending/pools",
		// Routers registry + routed-via 24h rollup. Rolling
		// observation kept fresh by the 1-min attribution
		// sweeper; a 60s edge cache stays inside that cadence.
		path == "/v1/aggregators",
		// Network-stats strip — single SQL query backing the
		// explorer's home network strip; cheap to cache.
		path == "/v1/network/stats",
		// Incident JSON list — embedded with the binary, only
		// changes on redeploy. (.atom variant sets its own header.)
		path == "/v1/incidents",
		// SAC wrapper map — operator-config, only changes on
		// process restart. Most cacheable surface in the API.
		path == "/v1/sac-wrappers",
		// Registry catalogue — issuer directory.
		path == "/v1/issuers",
		strings.HasPrefix(path, "/v1/issuers/"),
		// Accounts analytics — 30-min rollup snapshot; the public
		// catalogue band is well inside its real cadence.
		path == "/v1/accounts/stats",
		// Curated address labels — resynced from upstream at most
		// daily; the query string (address list) is the cache key.
		// Deliberately public despite the /v1/accounts/* siblings
		// being private: this is reference data, not per-account
		// state.
		path == "/v1/directory",
		// Multi-window delta strip. Refreshed every 5 min by the
		// change-summary worker; 60s edge cache stays well inside
		// that boundary, and 5 min s-maxage matches.
		strings.HasPrefix(path, "/v1/changes/"):
		if cdnEnabled {
			return "public, max-age=60, s-maxage=300"
		}
		return "public, max-age=60"

	// ─── Explorer account surface — same private, no-store as its
	// siblings ───────────────────────────────────────────────────
	// GET /v1/accounts, /v1/accounts/{g}, /v1/accounts/{g}/transactions,
	// /v1/accounts/{g}/operations, /v1/accounts/{g}/movements
	// (ADR-0048 D5), /v1/accounts/{g}/positions, /v1/accounts/{g}/trades,
	// and /v1/accounts/{g}/activity never had an explicit case here — they've always
	// fallen through to the conservative default below. Made EXPLICIT
	// (still private, no-store, no behavior change) when D5 added
	// /movements, so a future reviewer can see the account-surface
	// policy is a deliberate match to /v1/account/* (singular,
	// auth-tied) rather than an oversight: per-account listings are
	// keyset-paginated over the current lake tip, and an address is a
	// poor shared-CDN cache key regardless.
	case path == "/v1/accounts", strings.HasPrefix(path, "/v1/accounts/"):
		return "private, no-store"

	// ─── Default — be conservative ──────────────────────────────
	// Unknown path: don't accidentally let the CDN cache something
	// that turns out to be auth-tied later. Matches /v1/account/*
	// stance.
	default:
		return "private, no-store"
	}
}
