package middleware

import (
	"net/http"
	"regexp"
	"strings"
)

// CacheControl sets Cache-Control per route policy (ADR-0018); operator and tip routes are never shared-cached, and every problem+json
// writer MUST set `no-store` or the CDN caches the failure.
func CacheControl(next http.Handler) http.Handler {
	return CacheControlWithCDN(true)(next)
}

// CacheControlWithCDN returns the cache-control middleware; with cdnEnabled false only `max-age` is emitted on cacheable routes.
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

// perCallerHeaderStripper drops [perCallerHeaders] from a response a shared cache may reuse. Decided at commit time because handlers
// override Cache-Control in between. Stripped rather than Vary'd: a per-request key would make every response a CDN miss.
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

// WriteHeader commits on the final status; a 1xx interim response does not fix the final header set.
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

// Flush preserves http.Flusher for SSE handlers; a Flush before any WriteHeader commits an implicit 200.
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

// sharedCacheReusable reports whether a shared cache may store and re-serve the response (RFC 9111). Bare `private`, `no-store` and
// `no-cache` say no (no-cache revalidates, and the 304 restates the current request's headers); field-qualified forms do not count.
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

// ledgerDetailPath and txDetailPath match closed-ledger detail paths. The band stays modest (1 min client / 5 min CDN): a young ledger
// can be served before every projection for it has landed, so a long TTL could pin a partial view.
var (
	ledgerDetailPath = regexp.MustCompile(`^/v1/ledgers/[0-9]+(/transactions|/operations)?$`)
	txDetailPath     = regexp.MustCompile(`^/v1/tx/[0-9a-fA-F]{64}$`)
	// protocolTVLPath is /v1/protocols/{name}/tvl only, so the sibling directory and detail routes
	// keep their handler-set policy.
	protocolTVLPath = regexp.MustCompile(`^/v1/protocols/[^/]+/tvl$`)
	// contractDetailPath is /v1/contracts/{id} and its /interactions, /code-history and
	// /transfers children. /wasm is excluded: its handler sets its own directive.
	contractDetailPath = regexp.MustCompile(`^/v1/contracts/[^/]+(/interactions|/code-history|/transfers)?$`)
	// lendingReservesPath is /v1/lending/pools/{pool}/reserves.
	lendingReservesPath = regexp.MustCompile(`^/v1/lending/pools/[^/]+/reserves$`)
)

// ledgerPolicy classifies the operator probes and the explorer's ledger/tx surface; ok=false means fall through to the main switch.
// /v1/operations and /v1/contracts sit behind caches that refresh only ON a request, so a 60 s/300 s band would compound their age.
func ledgerPolicy(path string, cdnEnabled bool) (string, bool) {
	switch {
	// Operator probes: a cached probe is a lie. Here rather than in the main switch to keep
	// policyForPath under the gocyclo ceiling.
	case path == "/v1/healthz", path == "/v1/readyz", path == "/v1/version", path == "/metrics":
		return "no-store", true
	case ledgerDetailPath.MatchString(path), txDetailPath.MatchString(path):
		return closedLedgerPolicy(cdnEnabled), true
	// /v1/ledgers/at 404s for a ts past the tip until that ledger lands: short band.
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

// SLOPriceRoutes are the closed-bucket price surfaces with a very short shared cache; a shared TTL of d skews as_of by up to d, so
// s-maxage must stay inside the 150 s SLA probe bound (TestPolicyForPath_PriceSharedTTLIsBoundedByTheProbe).
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
		// Scam-gated like the routes above: on the 300 s band a CDN entry minted just before a
		// flag flips would serve the pre-flip price while the origin's withheld 404 is no-store.
		path == "/v1/price/at",
		path == "/v1/vwap",
		path == "/v1/twap":
		if cdnEnabled {
			return "public, max-age=30, s-maxage=5", true
		}
		return "public, max-age=30", true

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
		// Native (CAP-38) liquidity-pool reserves: current state, like /v1/pools/reserves.
		path == "/v1/liquidity-pools",
		// SDEX order-book depth: current offer state from an in-process book.
		path == "/v1/sdex/orderbook",
		// Per-pool DEX TVL: a 10-minute snapshot; the short band keeps a CDN entry from
		// outliving a refresh.
		protocolTVLPath.MatchString(path),
		// Lending-pool reserves: current contract state, like /v1/pools/reserves.
		lendingReservesPath.MatchString(path),
		// The pool list carries each pool's summary from that same reserves view.
		path == "/v1/lending/pools",
		// The non-Stellar half of /v1/assets: same live valuations, so the same band.
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

// policyForPath classifies a path into a Cache-Control directive. The more-specific prefix MUST win (`/v1/price/tip` is private,
// `/v1/price` public). With cdnEnabled false only `max-age` survives, so a CDN the operator lacks cannot cache anything.
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

	// ─── Magic-link auth + dashboard: same trust class as SEP-10. Explicit so a CDN cannot
	// cache /v1/auth/callback's session-cookie response; /v1/signup is state-changing.
	case strings.HasPrefix(path, "/v1/auth/"),
		strings.HasPrefix(path, "/v1/dashboard/"),
		path == "/v1/signup":
		return "private, no-store", true

	// ─── SSE streams: a long-lived stream is not cacheable; a CDN may buffer and replay it.
	case strings.HasPrefix(path, "/v1/price/stream"):
		return "no-store", true

	// ─── Methodology is mostly static prose; the atom feed is poll-cadence content.
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
		// Trailing-24h volume-by-source aggregate; same cadence as /v1/markets.
		path == "/v1/markets/sources",
		path == "/v1/pairs",
		// Auto-flagged MEV feed written every 5 min, so a 300 s edge entry adds one sweep of lag.
		path == "/v1/mev",
		// Pure strkey/hash-shape classification of ?q=, so the answer never changes.
		path == "/v1/search",
		path == "/v1/sources",
		strings.HasPrefix(path, "/v1/oracle/"),
		// Closed-bucket OHLCV (ADR-0015), same semantics as /v1/ohlc.
		path == "/v1/chart",
		path == "/v1/pools",
		// Embedded with the binary, changes only on redeploy (.atom sets its own header).
		path == "/v1/incidents",
		// Operator config, changes only on restart: the most cacheable surface.
		path == "/v1/sac-wrappers",
		path == "/v1/issuers",
		strings.HasPrefix(path, "/v1/issuers/"),
		// 30-min rollup snapshot, well inside the catalogue band.
		path == "/v1/accounts/stats",
		// Network-wide aggregate reference data, not per-account state. Exact path only: deeper
		// paths stay private, no-store.
		path == "/v1/accounts/creators",
		// Sponsor league table: same reasoning as its creator sibling.
		path == "/v1/accounts/sponsors",
		// Curated address labels, resynced at most daily. Deliberately public despite the
		// private /v1/accounts/* siblings: reference data, not per-account state.
		path == "/v1/directory",
		// Refreshed every 5 min by a worker, which the 5 min s-maxage matches.
		strings.HasPrefix(path, "/v1/changes/"),
		// Deliberately the longer band its /v1/rwa/assets sibling does not take: a daily series
		// behind a 10-minute TTL, not live valuations. The premium-to-NAV series is the same.
		path == "/v1/rwa/history",
		path == "/v1/rwa/premium":
		if cdnEnabled {
			return "public, max-age=60, s-maxage=300", true
		}
		return "public, max-age=60", true

	// ─── Explorer account surface — same private, no-store as its siblings ───
	// Explicit rather than the default so it reads as deliberate: per-account listings are
	// keyset-paginated over the current lake tip, and an address is a poor shared-CDN key.
	case path == "/v1/accounts", strings.HasPrefix(path, "/v1/accounts/"):
		return "private, no-store", true

	// ─── Live freeze + divergence state — never shared-cached ───
	// Consumers use these to decide whether a price is trustworthy, so an edge copy would keep a
	// recovered freeze reading as firing, or a new one as absent.
	case path == "/v1/anomalies", path == "/v1/divergence", path == "/v1/divergence/series":
		return "private, no-store", true

	}
	return "", false
}
