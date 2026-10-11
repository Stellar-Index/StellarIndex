package middleware

import (
	"net/http"
	"regexp"
	"strings"
)

// CacheControl sets Cache-Control per the route's policy (ADR-0018 surface
// model). `s-maxage` lets the CDN absorb bursts while `max-age` keeps clients
// fresh. Operator and tip routes are never shared-cached because caching would
// mask outages or change the tip's consistency contract. Scam-gated price reads
// get `s-maxage=5` so a shared entry cannot outlive a scam-withhold flip by
// more than one probe tick.
//
// A handler may override the directive by setting the header before writing.
// Every problem+json writer MUST set `Cache-Control: no-store`; otherwise an
// error inherits the route's public directive and the CDN caches the failure
// against the success key.
//
// Behaves like cdn_enabled=true; deployments without a CDN use
// [CacheControlWithCDN] to drop `s-maxage`.
func CacheControl(next http.Handler) http.Handler {
	return CacheControlWithCDN(true)(next)
}

// CacheControlWithCDN returns the cache-control middleware with the
// `s-maxage` (CDN-tier) directive controlled by `cdnEnabled`. When
// false, only `max-age` (client tier) is emitted on cacheable
// routes — appropriate for deployments without a CDN in front.
// `private, no-store` and `private, no-cache, must-revalidate`
// directives are unaffected (they are never CDN-cacheable).
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

// perCallerHeaders describe one request and one caller: the limiter's
// account for that caller and the request's correlation id. A shared
// cache replays a stored response to later callers without asking the
// origin, so it would hand one caller's budget and request id to
// everyone, as a limit no longer tied to any charge.
var perCallerHeaders = [...]string{
	"X-RateLimit-Limit",
	"X-RateLimit-Remaining",
	"X-RateLimit-Reset",
	HeaderRequestID,
}

// perCallerHeaderStripper drops [perCallerHeaders] from a response a
// shared cache may reuse. It decides when the header is committed, not
// when the middleware runs, because handlers and problem writers
// override Cache-Control in between. Stripped rather than listed in
// Vary: a per-request Vary key would make every response a CDN miss.
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

// sharedCacheReusable reports whether a shared cache may store the
// response and serve it again without the origin (RFC 9111 §3, §5.2.2).
// Bare `private` and `no-store` forbid storing; bare `no-cache` forces a
// revalidation whose 304 restates the current request's headers. The
// field-qualified forms leave the rest of the response reusable, so they
// do not count.
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

// ledgerDetailPath and txDetailPath match closed-ledger detail paths. A
// ledger's own row, its transaction and operation lists and a transaction
// by hash are IMMUTABLE once the ledger has closed, so they do not take the
// conservative default's `private, no-store`, which would make the
// explorer re-fetch a 71 KB transaction list on every visit to a ledger
// page. Their band is deliberately modest (1 min client / 5 min CDN), not
// a year: policyForPath knows nothing about the tip, and a ledger a few
// seconds old can be served before every downstream projection for it has
// landed, so a long TTL could pin a partial view. Five minutes rides out
// that lag and still absorbs the repeat-visit cost.
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

// ledgerPolicy classifies the operator probes and the explorer's ledger/tx
// surface. ok=false means "not one of mine, fall through to the main switch".
//
//   - A ledger's own row, its transaction/operation list and a transaction by
//     hash are IMMUTABLE once the ledger closes, but the band is deliberately
//     modest (1 min client / 5 min CDN): policyForPath knows nothing about the
//     tip, and a ledger a few seconds old can be served before every downstream
//     projection has landed, so a long TTL could pin a partial view.
//   - /v1/ledgers moves every ~5 s and /v1/network/throughput has a server-side
//     cache; both get the status-like short band.
//   - /v1/operations sits behind opsDirCache (10 s TTL + stale-while-revalidate)
//     which refreshes only ON a request, so at a low arrival rate an entry's age
//     is bounded by the inter-arrival gap, not the TTL; a 60 s/300 s band would
//     compound it.
//   - /v1/contracts (EXACT path) is the directory behind the same cache
//     (recentContractsCached); /v1/contracts/{id}, /interactions and
//     /code-history take the same band. /transfers is a cursorless latest-N
//     listing whose first page moves with every transfer: short band.
func ledgerPolicy(path string, cdnEnabled bool) (string, bool) {
	switch {
	// Operator endpoints — probed by systemd/Prometheus/uptime checks; a
	// cached probe is a lie. They sit here with the ledger cases rather
	// than in the main switch so policyForPath stays under the gocyclo
	// ceiling.
	case path == "/v1/healthz", path == "/v1/readyz", path == "/v1/version", path == "/metrics":
		return "no-store", true
	case ledgerDetailPath.MatchString(path), txDetailPath.MatchString(path):
		return closedLedgerPolicy(cdnEnabled), true
	// /v1/ledgers/at 404s for a ts past the tip until that ledger lands,
	// so it takes the short band rather than the closed-ledger one.
	case path == "/v1/ledgers", path == "/v1/ledgers/at", path == "/v1/network/throughput",
		path == "/v1/operations", path == "/v1/contracts", path == "/v1/contracts/stats",
		contractDetailPath.MatchString(path),
		// Network-stats strip: a 30s SWR cache carrying
		// latest_ledger, which advances every ~5s — the 300s catalogue
		// band would be 10x its own cache lifetime. Joins its
		// /v1/network/throughput sibling in the short band instead.
		path == "/v1/network/stats":
		if cdnEnabled {
			return "public, max-age=10, s-maxage=15", true
		}
		return "public, max-age=10", true
	}
	return "", false
}

// ─── Closed-bucket price surfaces: VERY short shared cache ──
// ADR-0015/0018 is a DETERMINISM contract (byte-identical for the same (pair,
// window, from_ts)), not a freshness bound. A shared cache serving a previous
// closed bucket breaks the "MOST RECENT closed bucket" clause the SLA probe
// measures (150 s target: 60 s bucket + 30 s CAGG end_offset + <=30 s schedule
// + runtime).
//
// A shared TTL of d adds d to the worst-case age of `observed_at`, makes `as_of`
// lie by up to d, and extends by d the window in which a pre-freeze price is
// served with `frozen=false`. s-maxage 60 can serve a bucket a full bucket behind
// origin (past the probe bound); 5 s stays inside it. `max-age` stays 30 s:
// private client reuse is the client's own copy.
//
// /v1/oracle/latest, /lastprice and /x_last_price are "last observed price"
// surfaces and /v1/oracle/prices is closed-bucket: none belong in the 300 s
// `/v1/oracle/` band. SLOPriceRoutes names every route above so
// TestPolicyForPath_PriceSharedTTLIsBoundedByTheProbe iterates the real set.
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
		// /v1/price/at, /v1/vwap and /v1/twap are the same
		// hazard on a different surface — each calls the scam gate
		// (writeIfScamWithheld in vwap.go/twap.go; lookupPriceAt in price_at.go), so
		// "closed-bucket price data looks cacheable" does not earn them
		// the 300 s catalogue band below. A CDN
		// entry minted a second before a scam flag flips serves the
		// pre-freeze price for up to 300 s after the flip, while the
		// origin's own withheld 404 is `no-store`. The short band bounds
		// that window the same way it bounds the SEP-40 passthroughs'.
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
		// Tokenized real-world assets. Its membership set is
		// rebuilt on a 10-minute in-process cadence, but every number
		// on it comes from the same catalogue read /v1/assets serves,
		// so it takes the same band as /v1/assets rather than the
		// longer catalogue one: a CDN entry must not outlive the
		// valuations it carries.
		path == "/v1/rwa/assets",
		// Stablecoin supply valued through the same listing pipeline.
		path == "/v1/stablecoins",
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
		path == "/v1/sdex/orderbook",
		// Per-pool DEX TVL drill-down — the valued counterpart of
		// /v1/pools/reserves, served from a 10-minute in-process
		// snapshot; the same short band keeps a CDN entry from
		// outliving a refresh while absorbing explorer polling.
		protocolTVLPath.MatchString(path),
		// Lending-pool reserves — current state decoded from the pool
		// contract's storage in the lake (ADR-0039), the same nature as
		// /v1/pools/reserves.
		lendingReservesPath.MatchString(path),
		// The non-Stellar half of the /v1/assets catalogue: same
		// wire shape and the same live valuations, so the same band.
		path == "/v1/external/assets",
		strings.HasPrefix(path, "/v1/external/assets/"),
		// Routers registry + routed-via 24h rollup. Wired to the raw
		// store with no in-process cache and the attribution sweeper keeps
		// it fresh on a 1-min cadence — a 60s edge entry (not the 300s
		// catalogue band) is what stays inside that cadence.
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
func policyForPath(path string, cdnEnabled bool) string {
	// Closed-ledger detail + the two fast-moving explorer reads
	// live in their own helper so this switch stays under the gocyclo
	// ceiling; see ledgerPolicy for the rationale on each band.
	if p, ok := ledgerPolicy(path, cdnEnabled); ok {
		return p
	}
	// The two SHORT bands: closed-bucket price surfaces (5s shared) and
	// current asset/pool state (60s shared). Lifted out of the switch to
	// stay under the gocyclo ceiling; see shortBandPolicy for why 5s and
	// not 60s on the price side.
	if p, ok := shortBandPolicy(path, cdnEnabled); ok {
		return p
	}
	if p, ok := routePolicy(path, cdnEnabled); ok {
		return p
	}
	// Default — be conservative: an unknown path must not let the CDN cache
	// something that turns out to be auth-tied later. A registered public GET
	// route must not rely on this;
	// TestPolicyForPath_EveryRegisteredGETRouteIsAdjudicated fails on one
	// that reaches it without an allowlist entry.
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
	// Caching the challenge would let a future request reuse a
	// nonce; caching the token would expose it to anyone the CDN
	// serves. Both unconditionally bypass cache.
	case strings.HasPrefix(path, "/v1/auth/sep10"):
		return "private, no-store", true

	// ─── Magic-link auth + dashboard — same trust class as SEP-10
	// /v1/auth/{login,callback,logout} + /v1/dashboard/keys* get an
	// explicit arm rather than the no-match branch: a CDN in front
	// of the API could otherwise cache /v1/auth/callback's
	// session-cookie response and re-issue it to subsequent
	// requests. /v1/signup is also a credential / state-changing
	// surface that must never cache. Routes that reach the default
	// are listed, with a reason each, in the test's
	// defaultPolicyAllowlist.
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
	// Tip has no cross-region consistency contract; caching
	// would shift the contract. Same for observations.
	case path == "/v1/price/tip",
		strings.HasPrefix(path, "/v1/price/tip/"),
		path == "/v1/observations",
		strings.HasPrefix(path, "/v1/observations/"):
		return "private, no-cache, must-revalidate", true

	// ─── Diagnostics — operator-facing live data ────────────────
	// /v1/diagnostics/cursors is polled every 15s by the explorer
	// /diagnostics page; caching would defeat the "watch the
	// indexer tick" UX. Same shape as tip/observations: tight
	// freshness, never CDN-cached.
	case strings.HasPrefix(path, "/v1/diagnostics/"):
		return "private, no-cache, must-revalidate", true

	// ─── Per-source health — same freshness class as diagnostics ──
	// /v1/sources/{name}/health serves the 15s-refreshed ingestion-
	// snapshot row; the explorer source page polls it. Note the
	// trailing slash: the exact-match `/v1/sources` catalogue path in
	// the closed-bucket block below is unaffected.
	case strings.HasPrefix(path, "/v1/sources/"):
		return "private, no-cache, must-revalidate", true

	// ─── Status — customer-facing health rollup ─────────────────
	// /v1/status is what the explorer /status page polls every 30 s
	// and what monitoring dashboards (and the smoke timer) poll on a
	// longer interval. A 10 s cache absorbs the polling fan-out
	// without delaying alert-state propagation enough to matter —
	// the underlying signals (Prometheus heartbeats, incident counts)
	// already have 15 s scrape granularity. /v1/status/notices is the
	// active operator banner list the same page renders beside it.
	case path == "/v1/status", path == "/v1/status/notices":
		if cdnEnabled {
			return "public, max-age=10, s-maxage=15", true
		}
		return "public, max-age=10", true

	// ─── Historical / closed-bucket / catalogue — longer cache ──
	// Closed buckets are immutable per ADR-0015 but the
	// trailing-edge boundary advances; s-maxage=300 caps how long
	// a CDN entry can lag the boundary.
	case strings.HasPrefix(path, "/v1/history"),
		// /v1/price/at, /v1/vwap and /v1/twap are in shortBandPolicy
		// — each is a scam-gated price surface and must
		// not outlive a withhold flip in a shared cache the way an
		// immutable OHLC bucket safely can.
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
		// Account-creator league table — a rollup snapshot on the
		// same cadence, and network-wide aggregate reference data rather
		// than per-account state, so it takes the public band its
		// /v1/accounts/* siblings deliberately do not. Exact path only:
		// anything deeper falls through to private, no-store.
		path == "/v1/accounts/creators",
		// Sponsor league table — same rollup cadence and the same
		// aggregate-reference-data reasoning as its creator sibling.
		path == "/v1/accounts/sponsors",
		// Curated address labels — resynced from upstream at most
		// daily; the query string (address list) is the cache key.
		// Deliberately public despite the /v1/accounts/* siblings
		// being private: this is reference data, not per-account
		// state.
		path == "/v1/directory",
		// Multi-window delta strip. Refreshed every 5 min by the
		// change-summary worker; 60s edge cache stays well inside
		// that boundary, and 5 min s-maxage matches.
		strings.HasPrefix(path, "/v1/changes/"),
		// RWA value-over-time. Deliberately the LONGER band its
		// /v1/rwa/assets sibling does not take: that surface carries
		// live valuations whose CDN entry must not outlive them, while
		// this one is a DAILY series assembled behind a 10-minute TTL
		// — its newest bucket cannot move faster than the oracle
		// publishes into today.
		path == "/v1/rwa/history",
		// The premium-to-NAV series, on the same grounds: a daily
		// series behind the same 10-minute assembly TTL.
		path == "/v1/rwa/premium":
		if cdnEnabled {
			return "public, max-age=60, s-maxage=300", true
		}
		return "public, max-age=60", true

	// ─── Explorer account surface — same private, no-store as its
	// siblings ───────────────────────────────────────────────────
	// GET /v1/accounts, /v1/accounts/{g}, /v1/accounts/{g}/transactions,
	// /v1/accounts/{g}/operations, /v1/accounts/{g}/movements
	// (ADR-0048 D5), /v1/accounts/{g}/positions, /v1/accounts/{g}/trades,
	// /v1/accounts/{g}/activity, /v1/accounts/{g}/graph and
	// /v1/accounts/{g}/graph/history take the conservative
	// default's private, no-store, but EXPLICITLY, so a reviewer
	// can see the account-surface
	// policy is a deliberate match to /v1/account/* (singular,
	// auth-tied) rather than an oversight: per-account listings are
	// keyset-paginated over the current lake tip, and an address is a
	// poor shared-CDN cache key regardless.
	case path == "/v1/accounts", strings.HasPrefix(path, "/v1/accounts/"):
		return "private, no-store", true

	// ─── Live freeze + divergence state — never shared-cached ───
	// /v1/anomalies reports the LIVE firing count and, with
	// ?firing=true, the freezes firing right now (ADR-0019);
	// /v1/divergence is the current cross-reference board and
	// /series its history companion. A consumer reads these to decide
	// whether a price is trustworthy, so an edge copy would extend the
	// window in which a recovered freeze still reads as firing, or a
	// new one reads as absent — the same hazard shortBandPolicy bounds
	// to 5 s for /v1/price's `frozen` flag. Explicit, not the default.
	case path == "/v1/anomalies", path == "/v1/divergence", path == "/v1/divergence/series":
		return "private, no-store", true

	}
	return "", false
}
