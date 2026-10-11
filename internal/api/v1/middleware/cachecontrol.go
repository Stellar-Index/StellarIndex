package middleware

import (
	"net/http"
	"regexp"
	"strings"
)

// CacheControl sets Cache-Control per the route's policy (ADR-0018 surface model).
// Operator and tip routes are never shared-cached, and scam-gated price reads get a
// 5 s `s-maxage` so a shared entry cannot outlive a scam-withhold flip by more than a probe tick.
//
// A handler may override the directive by setting the header before writing. Every
// problem+json writer MUST set `Cache-Control: no-store`; otherwise an error inherits the
// route's public directive and the CDN caches the failure against the success key.
//
// Behaves like cdn_enabled=true; use [CacheControlWithCDN] to drop `s-maxage`.
func CacheControl(next http.Handler) http.Handler {
	return CacheControlWithCDN(true)(next)
}

// CacheControlWithCDN returns the cache-control middleware; when cdnEnabled is false only
// `max-age` is emitted on cacheable routes. Private directives are unaffected.
func CacheControlWithCDN(cdnEnabled bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", policyForPath(r.URL.Path, cdnEnabled))
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				w = &perCallerHeaderStripper{ResponseWriter: w}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// perCallerHeaders describe one caller and request. A shared cache replays a stored response
// without asking the origin, so it would hand one caller's budget and request id to everyone.
var perCallerHeaders = [...]string{
	"X-RateLimit-Limit",
	"X-RateLimit-Remaining",
	"X-RateLimit-Reset",
	HeaderRequestID,
}

// perCallerHeaderStripper drops [perCallerHeaders] from a response a shared cache may reuse.
// It decides at commit time, not when the middleware runs, because handlers and problem writers
// override Cache-Control in between. Stripped rather than Vary'd: a per-request key would make
// every response a CDN miss.
type perCallerHeaderStripper struct {
	http.ResponseWriter
	committed bool
}

func (s *perCallerHeaderStripper) commit() {
	if s.committed {
		return
	}
	s.committed = true
	h := s.Header()
	if !sharedCacheReusable(h.Values("Cache-Control")) {
		return
	}
	for _, name := range perCallerHeaders {
		h.Del(name)
	}
}

// WriteHeader commits on the final status; a 1xx interim response does
// not fix the final header set.
func (s *perCallerHeaderStripper) WriteHeader(status int) {
	if status >= http.StatusOK {
		s.commit()
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *perCallerHeaderStripper) Write(p []byte) (int, error) {
	s.commit()
	return s.ResponseWriter.Write(p)
}

// Flush preserves http.Flusher for SSE handlers; a Flush before any
// WriteHeader commits an implicit 200.
func (s *perCallerHeaderStripper) Flush() {
	s.commit()
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.NewResponseController.
func (s *perCallerHeaderStripper) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// sharedCacheReusable reports whether a shared cache may store the response and serve it again
// without the origin (RFC 9111). Bare `private`, `no-store` and `no-cache` say no (no-cache
// revalidates, and the 304 restates the current request's headers); field-qualified forms do not count.
func sharedCacheReusable(values []string) bool {
	for _, v := range values {
		for _, d := range strings.Split(v, ",") {
			switch strings.ToLower(strings.TrimSpace(d)) {
			case "private", "no-store", "no-cache":
				return false
			}
		}
	}
	return true
}

// ledgerDetailPath and txDetailPath match closed-ledger detail paths, which are immutable once
// the ledger closes. Their band is deliberately modest (1 min client / 5 min CDN), not a year:
// policyForPath knows nothing about the tip, and a young ledger can be served before every
// projection for it has landed, so a long TTL could pin a partial view.
var (
	ledgerDetailPath = regexp.MustCompile(`^/v1/ledgers/[0-9]+(/transactions|/operations)?$`)
	txDetailPath     = regexp.MustCompile(`^/v1/tx/[0-9a-fA-F]{64}$`)
	// protocolTVLPath is the per-pool DEX TVL drill-down
	// (/v1/protocols/{name}/tvl) — one segment for the protocol
	// name, nothing after /tvl, so the directory row and detail routes
	// beside it keep the handler-set policy they already have.
	protocolTVLPath = regexp.MustCompile(`^/v1/protocols/[^/]+/tvl$`)
	// contractDetailPath is /v1/contracts/{id} and its /interactions,
	// /code-history and /transfers children. /wasm is excluded: its handler
	// sets its own directive.
	contractDetailPath = regexp.MustCompile(`^/v1/contracts/[^/]+(/interactions|/code-history|/transfers)?$`)
	// lendingReservesPath is /v1/lending/pools/{pool}/reserves.
	lendingReservesPath = regexp.MustCompile(`^/v1/lending/pools/[^/]+/reserves$`)
)

// ledgerPolicy classifies the operator probes and the explorer's ledger/tx surface.
// ok=false means "not one of mine, fall through to the main switch".
//
//   - Closed-ledger reads take the modest band described at ledgerDetailPath.
//   - /v1/ledgers moves every ~5 s and /v1/network/throughput has a server-side cache; both
//     get the short band.
//   - /v1/operations and /v1/contracts sit behind caches that refresh only ON a request, so at
//     a low arrival rate an entry's age is bounded by the inter-arrival gap, not the TTL; a
//     60 s/300 s band would compound it. /transfers is a cursorless latest-N listing: short band.
func ledgerPolicy(path string, cdnEnabled bool) (string, bool) {
	switch {
	// Operator probes: a cached probe is a lie. Here rather than in the main switch to keep
	// policyForPath under the gocyclo ceiling.
	case path == "/v1/healthz", path == "/v1/readyz", path == "/v1/version", path == "/metrics":
		return "no-store", true
	case ledgerDetailPath.MatchString(path), txDetailPath.MatchString(path):
		return closedLedgerPolicy(cdnEnabled), true
	// /v1/ledgers/at 404s for a ts past the tip until that ledger lands,
	// so it takes the short band rather than the closed-ledger one.
	case path == "/v1/ledgers", path == "/v1/ledgers/at", path == "/v1/network/throughput",
		path == "/v1/operations", path == "/v1/contracts", path == "/v1/contracts/stats",
		contractDetailPath.MatchString(path),
		// Carries latest_ledger (advances ~5 s), so the 300 s catalogue band would be
		// 10x its own cache lifetime.
		path == "/v1/network/stats":
		if cdnEnabled {
			return "public, max-age=10, s-maxage=15", true
		}
		return "public, max-age=10", true
	}
	return "", false
}

// SLOPriceRoutes are the closed-bucket price surfaces, which get a very short shared cache.
// ADR-0015/0018 is a determinism contract, not a freshness bound, but a shared cache serving a
// previous bucket breaks the "most recent closed bucket" clause the SLA probe measures (150 s).
// A shared TTL of d adds d to the age of `observed_at`, makes `as_of` lie by up to d, and
// extends by d the window in which a pre-freeze price is served with `frozen=false`; s-maxage 60
// exceeds the probe bound, 5 s stays inside it. None belong in the 300 s `/v1/oracle/` band.
// TestPolicyForPath_PriceSharedTTLIsBoundedByTheProbe iterates this list.
var SLOPriceRoutes = []string{
	"/v1/price",
	"/v1/price/batch",
	"/v1/price/changes",
	"/v1/oracle/latest",
	"/v1/oracle/lastprice",
	"/v1/oracle/prices",
	"/v1/oracle/x_last_price",
}

func shortBandPolicy(path string, cdnEnabled bool) (string, bool) {
	switch {
	case path == "/v1/price",
		strings.HasPrefix(path, "/v1/price/batch"),
		path == "/v1/price/changes",
		path == "/v1/oracle/latest",
		path == "/v1/oracle/lastprice",
		path == "/v1/oracle/prices",
		path == "/v1/oracle/x_last_price",
		// Scam-gated like the routes above, so they must not take the 300 s catalogue band:
		// a CDN entry minted just before a scam flag flips would serve the pre-flip price
		// for up to 300 s while the origin's withheld 404 is `no-store`.
		path == "/v1/price/at",
		path == "/v1/vwap",
		path == "/v1/twap":
		if cdnEnabled {
			return "public, max-age=30, s-maxage=5", true
		}
		return "public, max-age=30", true

	// Updates on every bucket close; CDN entry should turn over
	// inside one bucket so consumers see fresh closed-bucket data.
	// ─── Current asset / pool state — short cache ───────────────
	case path == "/v1/assets",
		strings.HasPrefix(path, "/v1/assets/"),
		// Carries the same live valuations as /v1/assets, so a CDN entry must not outlive
		// them; same band rather than the longer catalogue one.
		path == "/v1/rwa/assets",
		// Stablecoin supply valued through the same listing pipeline.
		path == "/v1/stablecoins",
		// Current contract state, changes every ledger; the short band absorbs polling
		// fan-out. Exact-match: the /v1/pools listing keeps the catalogue band below.
		path == "/v1/pools/reserves",
		// Native (CAP-38) liquidity-pool reserves — same CURRENT-lake-
		// state nature as /v1/pools/reserves (both listing + ?pool=).
		path == "/v1/liquidity-pools",
		// SDEX order-book depth: current offer state from an in-process book.
		path == "/v1/sdex/orderbook",
		// Per-pool DEX TVL: a 10-minute snapshot; the short band keeps a CDN entry from
		// outliving a refresh.
		protocolTVLPath.MatchString(path),
		// Lending-pool reserves — current state decoded from the pool
		// contract's storage in the lake (ADR-0039), the same nature as
		// /v1/pools/reserves.
		lendingReservesPath.MatchString(path),
		// The non-Stellar half of the /v1/assets catalogue: same
		// wire shape and the same live valuations, so the same band.
		path == "/v1/external/assets",
		strings.HasPrefix(path, "/v1/external/assets/"),
		// Refreshed on a 1-min cadence, so a 60 s edge entry (not the 300 s band) stays inside it.
		path == "/v1/aggregators":
		if cdnEnabled {
			return "public, max-age=30, s-maxage=60", true
		}
		return "public, max-age=30", true
	}
	return "", false
}

// closedLedgerPolicy is the band for reads of one closed ledger.
func closedLedgerPolicy(cdnEnabled bool) string {
	if cdnEnabled {
		return "public, max-age=60, s-maxage=300"
	}
	return "public, max-age=60"
}

// policyForPath classifies a request path into a Cache-Control directive; package scope lets
// tests pin the policy table without a handler.
//
// Order matters: the more-specific prefix MUST win. `/v1/price/tip` is private and `/v1/price`
// is public, so the tip rule must run first.
//
// With cdnEnabled false only `max-age` survives, so a CDN the operator does not have cannot
// cache anything.
func policyForPath(path string, cdnEnabled bool) string {
	// Split into helpers to stay under the gocyclo ceiling; see ledgerPolicy.
	if p, ok := ledgerPolicy(path, cdnEnabled); ok {
		return p
	}
	// Short bands: price surfaces (5 s shared) and current asset/pool state (60 s shared); see
	// SLOPriceRoutes for why price is 5 s.
	if p, ok := shortBandPolicy(path, cdnEnabled); ok {
		return p
	}
	if p, ok := routePolicy(path, cdnEnabled); ok {
		return p
	}
	// Conservative default: an unknown path must not let the CDN cache something that turns out
	// to be auth-tied. TestPolicyForPath_EveryRegisteredGETRouteIsAdjudicated fails on a
	// registered public GET route that reaches it without an allowlist entry.
	return defaultPolicy
}

// defaultPolicy is what an unclassified path gets.
const defaultPolicy = "private, no-store"

// routePolicy is policyForPath's main table. ok=false means no arm matched.
func routePolicy(path string, cdnEnabled bool) (string, bool) {
	switch {
	// ─── Operator endpoints — never cached ──────────────────────

	// ─── Account endpoints — auth-tied, MUST NOT hit CDN ────────
	case strings.HasPrefix(path, "/v1/account/"):
		return "private, no-store", true

	// ─── SEP-10 Web Auth — credential exchange MUST NOT hit CDN ─
	// Caching the challenge would let a later request reuse a nonce, and caching the token
	// would expose it to anyone the CDN serves.
	case strings.HasPrefix(path, "/v1/auth/sep10"):
		return "private, no-store", true

	// ─── Magic-link auth + dashboard — same trust class as SEP-10
	// An explicit arm rather than the default: a CDN could otherwise cache
	// /v1/auth/callback's session-cookie response and re-issue it to later requests.
	// /v1/signup is a credential and state-changing surface.
	case strings.HasPrefix(path, "/v1/auth/"),
		strings.HasPrefix(path, "/v1/dashboard/"),
		path == "/v1/signup":
		return "private, no-store", true

	// ─── SSE streams — bypass CDN cache; the response is a long-
	// lived event stream, not a cacheable body. Without this CDNs
	// may try to buffer + replay.
	case strings.HasPrefix(path, "/v1/price/stream"):
		return "no-store", true

	// ─── Public-but-policy-opinionated paths with an explicit
	// case. Methodology page is mostly
	// static prose; the atom feed is poll-cadence content.
	case path == "/v1/methodology":
		if cdnEnabled {
			return "public, max-age=300, s-maxage=600", true
		}
		return "public, max-age=300", true
	case path == "/v1/incidents.atom":
		if cdnEnabled {
			return "public, max-age=60, s-maxage=120", true
		}
		return "public, max-age=60", true

	// ─── Tip + observations — private surfaces (ADR-0018) ───────
	// Tip has no cross-region consistency contract; caching would shift it.
	case path == "/v1/price/tip",
		strings.HasPrefix(path, "/v1/price/tip/"),
		path == "/v1/observations",
		strings.HasPrefix(path, "/v1/observations/"):
		return "private, no-cache, must-revalidate", true

	// ─── Diagnostics — operator-facing live data ────────────────
	// Polled every 15 s to watch the indexer tick; caching would defeat that.
	case strings.HasPrefix(path, "/v1/diagnostics/"):
		return "private, no-cache, must-revalidate", true

	// ─── Per-source health — same freshness class as diagnostics ──
	// The trailing slash matters: the exact-match `/v1/sources` catalogue path below is
	// unaffected.
	case strings.HasPrefix(path, "/v1/sources/"):
		return "private, no-cache, must-revalidate", true

	// ─── Status — customer-facing health rollup ─────────────────
	// A 10 s cache absorbs polling fan-out without delaying alert-state propagation, since the
	// underlying signals already have 15 s scrape granularity.
	case path == "/v1/status", path == "/v1/status/notices":
		if cdnEnabled {
			return "public, max-age=10, s-maxage=15", true
		}
		return "public, max-age=10", true

	// ─── Historical / closed-bucket / catalogue — longer cache ──
	// Closed buckets are immutable (ADR-0015) but the trailing edge advances; s-maxage=300 caps
	// how long a CDN entry can lag it.
	case strings.HasPrefix(path, "/v1/history"),
		// /v1/price/at, /v1/vwap and /v1/twap are in shortBandPolicy: scam-gated, so they must
		// not outlive a withhold flip the way an immutable OHLC bucket safely can.
		path == "/v1/ohlc",
		path == "/v1/markets",
		// Trailing-24h volume-by-source aggregate behind the market and
		// asset pages; the same cadence as /v1/markets.
		path == "/v1/markets/sources",
		path == "/v1/pairs",
		// Auto-flagged MEV feed. The detector worker writes it every
		// 5 min, so a 300 s edge entry adds at most one sweep of lag.
		path == "/v1/mev",
		// Pure strkey/hash-shape classification of ?q= — no lake read,
		// so the answer for a given query never changes.
		path == "/v1/search",
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
		// Network-wide aggregate reference data, not per-account state, so it takes the public
		// band its /v1/accounts/* siblings do not. Exact path only: deeper paths stay
		// private, no-store.
		path == "/v1/accounts/creators",
		// Sponsor league table — same rollup cadence and the same
		// aggregate-reference-data reasoning as its creator sibling.
		path == "/v1/accounts/sponsors",
		// Curated address labels, resynced at most daily. Deliberately public despite the
		// private /v1/accounts/* siblings: reference data, not per-account state.
		path == "/v1/directory",
		// Refreshed every 5 min by a worker, which the 5 min s-maxage matches.
		strings.HasPrefix(path, "/v1/changes/"),
		// Deliberately the longer band its /v1/rwa/assets sibling does not take: this is a
		// daily series behind a 10-minute TTL, not live valuations.
		path == "/v1/rwa/history",
		// The premium-to-NAV series, on the same grounds: a daily
		// series behind the same 10-minute assembly TTL.
		path == "/v1/rwa/premium":
		if cdnEnabled {
			return "public, max-age=60, s-maxage=300", true
		}
		return "public, max-age=60", true

	// ─── Explorer account surface — same private, no-store as its siblings ───
	// Explicit rather than falling to the default, so the match with /v1/account/* (singular,
	// auth-tied) reads as deliberate: per-account listings are keyset-paginated over the
	// current lake tip, and an address is a poor shared-CDN cache key regardless.
	case path == "/v1/accounts", strings.HasPrefix(path, "/v1/accounts/"):
		return "private, no-store", true

	// ─── Live freeze + divergence state — never shared-cached ───
	// Consumers read these to decide whether a price is trustworthy, so an edge copy would
	// extend the window in which a recovered freeze still reads as firing, or a new one as
	// absent; the same hazard shortBandPolicy bounds to 5 s for `frozen`.
	case path == "/v1/anomalies", path == "/v1/divergence", path == "/v1/divergence/series":
		return "private, no-store", true

	}
	return "", false
}
