package middleware

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestPolicyForPath_PinsDirectives — the policy table is part of
// the API contract (CDN configs reference these strings). Pinning
// every documented path against its expected directive guards
// against a typo flipping a public-cacheable endpoint to
// no-store at scale.
func TestPolicyForPath_PinsDirectives(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		// Operator endpoints
		{"/v1/healthz", "no-store"},
		{"/v1/readyz", "no-store"},
		{"/v1/version", "no-store"},
		{"/metrics", "no-store"},

		// Status rollup — short public cache so the explorer polling
		// fan-out doesn't multiply against the API
		{"/v1/status", "public, max-age=10, s-maxage=15"},

		// Closed-ledger detail is immutable → modest public band;
		// the list + throughput move fast → status-like short band. The
		// account surface stays private (guards that the new regexps do
		// not over-match).
		{"/v1/ledgers/64000000", "public, max-age=60, s-maxage=300"},
		{"/v1/ledgers/64000000/transactions", "public, max-age=60, s-maxage=300"},
		{"/v1/ledgers/64000000/operations", "public, max-age=60, s-maxage=300"},
		{"/v1/tx/" + strings.Repeat("ab", 32), "public, max-age=60, s-maxage=300"},
		{"/v1/tx/notahash", "private, no-store"},
		{"/v1/ledgers", "public, max-age=10, s-maxage=15"},
		{"/v1/network/throughput", "public, max-age=10, s-maxage=15"},
		// Not the 300s catalogue band, 10x its own 30s SWR cache
		// lifetime. Joins its /v1/network/throughput sibling instead.
		{"/v1/network/stats", "public, max-age=10, s-maxage=15"},
		// The operations directory is /v1/ledgers' sibling listing and
		// joins the same band. Without a case it would ship
		// `private, no-store` from the default.
		{"/v1/operations", "public, max-age=10, s-maxage=15"},
		{"/v1/contracts", "public, max-age=10, s-maxage=15"},
		{"/v1/contracts/stats", "public, max-age=10, s-maxage=15"},
		{"/v1/ledgers/64000000/transactions/extra", "private, no-store"},

		// Account — auth-tied
		{"/v1/account/me", "private, no-store"},
		{"/v1/account/usage", "private, no-store"},
		{"/v1/account/keys", "private, no-store"},

		// SEP-10 Web Auth — credential exchange MUST NOT hit CDN
		{"/v1/auth/sep10/challenge", "private, no-store"},
		{"/v1/auth/sep10/token", "private, no-store"},

		// Tip + observations — private surfaces
		{"/v1/price/tip", "private, no-cache, must-revalidate"},
		{"/v1/price/tip/stream", "private, no-cache, must-revalidate"},
		{"/v1/observations", "private, no-cache, must-revalidate"},
		{"/v1/observations/stream", "private, no-cache, must-revalidate"},

		// The other two SSE streams. The SSE writer keeps whatever this
		// sets (stream_cache_control_test.go asserts the wire header).
		{"/v1/price/stream", "no-store"},
		{"/v1/ledger/stream", "private, no-store"},

		// Closed-bucket price surfaces — 5s shared cache. The
		// 150s SLA-probe freshness target leaves no room for
		// s-maxage=60: it can serve a bucket a full bucket behind
		// origin (age <= 210s) and a stale frozen/confidence for two
		// aggregator ticks. max-age stays 30s — a client's own copy is
		// not a shared-cache lie.
		{"/v1/price", "public, max-age=30, s-maxage=5"},
		{"/v1/price/batch", "public, max-age=30, s-maxage=5"},
		// Multi-horizon change strip tracks the current-price band
		// (anchor moves on every bucket close).
		{"/v1/price/changes", "public, max-age=30, s-maxage=5"},
		// Scam-gated price surfaces — same band as
		// /v1/price: a shared cache entry must not outlive a
		// scam-withhold flip.
		{"/v1/price/at", "public, max-age=30, s-maxage=5"},
		{"/v1/vwap", "public, max-age=30, s-maxage=5"},
		{"/v1/twap", "public, max-age=30, s-maxage=5"},

		// Current asset detail — short cache
		{"/v1/assets", "public, max-age=30, s-maxage=60"},
		{"/v1/assets/native", "public, max-age=30, s-maxage=60"},
		{"/v1/assets/USDC-GA5Z/metadata", "public, max-age=30, s-maxage=60"},

		// The RWA surfaces take DIFFERENT bands, and the split is the
		// point: /v1/rwa/assets carries live valuations, so a CDN entry
		// must not outlive them, while the two daily series sit behind
		// a 10-minute assembly TTL.
		{"/v1/rwa/assets", "public, max-age=30, s-maxage=60"},
		{"/v1/stablecoins", "public, max-age=30, s-maxage=60"},
		{"/v1/rwa/history", "public, max-age=60, s-maxage=300"},
		{"/v1/rwa/premium", "public, max-age=60, s-maxage=300"},

		// Historical / closed-bucket
		{"/v1/history", "public, max-age=60, s-maxage=300"},
		{"/v1/history/since-inception", "public, max-age=60, s-maxage=300"},
		// /v1/price/at, /v1/vwap and /v1/twap are scam-gated price
		// surfaces and take the short band; see
		// TestPolicyForPath_PinsDirectives' shortBandPolicy cases below.
		{"/v1/ohlc", "public, max-age=60, s-maxage=300"},
		{"/v1/markets", "public, max-age=60, s-maxage=300"},
		{"/v1/pairs", "public, max-age=60, s-maxage=300"},
		{"/v1/sources", "public, max-age=60, s-maxage=300"},
		// NB /v1/oracle/latest, /v1/oracle/lastprice, /v1/oracle/prices and
		// /v1/oracle/x_last_price are NOT here — they left the catalogue
		// band; see
		// TestPolicyForPath_OracleLatestIsNotTheOraclePrefixBand and
		// TestPolicyForPath_OracleSEP40PassthroughsShareTheShortBand.

		// Registry catalogues + change-summary
		{"/v1/issuers", "public, max-age=60, s-maxage=300"},
		{"/v1/issuers/GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "public, max-age=60, s-maxage=300"},
		{"/v1/changes/coin/stellar", "public, max-age=60, s-maxage=300"},
		{"/v1/changes/pair/native:USDC", "public, max-age=60, s-maxage=300"},

		// Chart + lending + network-stats + sac-wrappers + incidents
		// JSON + pools — all read endpoints in the long catalogue
		// cache band.
		{"/v1/chart", "public, max-age=60, s-maxage=300"},
		{"/v1/lending/pools", "public, max-age=60, s-maxage=300"},
		{"/v1/sac-wrappers", "public, max-age=60, s-maxage=300"},
		{"/v1/incidents", "public, max-age=60, s-maxage=300"},
		{"/v1/pools", "public, max-age=60, s-maxage=300"},

		// Not the 300s catalogue band, despite its own comment
		// arguing for a 60s edge cache to match the 1-min attribution
		// sweeper.
		{"/v1/aggregators", "public, max-age=30, s-maxage=60"},

		// Pool reserves — current contract state; shorter band than
		// the /v1/pools listing (exact-match case wins).
		{"/v1/pools/reserves", "public, max-age=30, s-maxage=60"},
		{"/v1/sdex/orderbook", "public, max-age=30, s-maxage=60"},
		// Native (CAP-38) liquidity-pool reserves — same short band.
		{"/v1/liquidity-pools", "public, max-age=30, s-maxage=60"},
		// Per-pool DEX TVL drill-down — the valued counterpart of
		// /v1/pools/reserves; same short band. Only the exact
		// {name}/tvl shape: the directory row and detail beside it
		// keep their handler-set policy, and a deeper path is not it.
		{"/v1/protocols/soroswap/tvl", "public, max-age=30, s-maxage=60"},
		{"/v1/protocols/soroswap/tvl/extra", "private, no-store"},

		// Directory labels — slow-moving reference data in the public
		// catalogue band; sibling /v1/accounts/* stays private (the
		// account surface is deliberately no-store; this is reference
		// data keyed on the query-string address list).
		{"/v1/directory", "public, max-age=60, s-maxage=300"},

		// Account-creator league table — a rollup snapshot of
		// network-wide aggregate reference data, so it takes the public
		// catalogue band while the per-account surface around it stays
		// private. Exact path only: anything deeper must fall through to
		// the conservative default, or a future /v1/accounts/creators/{g}
		// per-account drill-down would inherit a public policy nobody
		// chose for it.
		{"/v1/accounts/creators", "public, max-age=60, s-maxage=300"},
		{"/v1/accounts/creators/extra", "private, no-store"},
		{"/v1/accounts/sponsors", "public, max-age=60, s-maxage=300"},
		{"/v1/accounts/sponsors/extra", "private, no-store"},

		// Diagnostics — operator-facing live data, never CDN-cached
		{"/v1/diagnostics/cursors", "private, no-cache, must-revalidate"},
		{"/v1/diagnostics/archive", "private, no-cache, must-revalidate"},

		// Per-source health — 15s-snapshot data, same class as
		// diagnostics. The exact-match catalogue path /v1/sources
		// above stays public-cacheable.
		{"/v1/sources/kraken/health", "private, no-cache, must-revalidate"},

		// Unknown — conservative default
		{"/v1/something-new", "private, no-store"},
		{"/", "private, no-store"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			if got := policyForPath(tc.path, true); got != tc.want {
				t.Errorf("policyForPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestPolicyForPath_TipBeatsPriceGenericPrefix — /v1/price/tip
// shares the /v1/price prefix; the tip rule MUST match first
// (private, no-cache) so a tip request never lands in a public CDN
// cache. Regression guard against re-ordering the switch.
func TestPolicyForPath_TipBeatsPriceGenericPrefix(t *testing.T) {
	tip := policyForPath("/v1/price/tip", true)
	price := policyForPath("/v1/price", true)
	if tip == price {
		t.Errorf("tip and price share directive %q — tip rule must run first", tip)
	}
	if tip != "private, no-cache, must-revalidate" {
		t.Errorf("/v1/price/tip = %q, want private no-cache", tip)
	}
}

// TestCacheControl_Middleware_SetsHeaderBeforeHandler — handlers
// see the header already on the writer; they CAN override it but
// the default is in place by the time they run.
func TestCacheControl_Middleware_SetsHeaderBeforeHandler(t *testing.T) {
	var observedAtHandler string
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		observedAtHandler = w.Header().Get("Cache-Control")
		w.WriteHeader(http.StatusOK)
	})
	mw := CacheControl(inner)

	req := httptest.NewRequest(http.MethodGet, "/v1/markets", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if observedAtHandler == "" {
		t.Error("handler saw empty Cache-Control; middleware must set it before next.ServeHTTP")
	}
	want := "public, max-age=60, s-maxage=300"
	if observedAtHandler != want {
		t.Errorf("handler saw %q, want %q", observedAtHandler, want)
	}
	if got := rec.Header().Get("Cache-Control"); got != want {
		t.Errorf("response Cache-Control = %q, want %q", got, want)
	}
}

// TestCacheControl_Middleware_HandlerOverrideWins — handlers that
// need to deviate (e.g. Etag-driven 304s) can overwrite the
// directive after the middleware ran. Verify the override survives.
func TestCacheControl_Middleware_HandlerOverrideWins(t *testing.T) {
	const override = "public, max-age=86400, immutable"
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", override)
		w.WriteHeader(http.StatusOK)
	})
	mw := CacheControl(inner)

	req := httptest.NewRequest(http.MethodGet, "/v1/healthz", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if got := rec.Header().Get("Cache-Control"); got != override {
		t.Errorf("override lost: Cache-Control = %q, want %q", got, override)
	}
}

// TestCacheControl_Middleware_AppliesToErrorResponses — a handler
// that 4xxs still gets the route's cache directive applied. CDNs
// are expected to refuse to cache 5xx via origin config; this test
// pins that the middleware itself doesn't strip the directive on
// non-2xx responses.
func TestCacheControl_Middleware_AppliesToErrorResponses(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad input", http.StatusBadRequest)
	})
	mw := CacheControl(inner)

	req := httptest.NewRequest(http.MethodGet, "/v1/markets", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60, s-maxage=300" {
		t.Errorf("Cache-Control on 400 = %q, want public max-age=60 s-maxage=300", got)
	}
}

// TestPolicyForPath_CDNDisabled — operators without a CDN in front
// of the API set api.cdn_enabled=false; the middleware must drop
// `s-maxage` from cacheable directives so a CDN they don't have
// can't cache anything. private + no-store directives are
// unaffected (they were never CDN-cacheable anyway).
func TestPolicyForPath_CDNDisabled(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		// Cacheable routes lose s-maxage.
		{"/v1/price", "public, max-age=30"},
		{"/v1/price/batch", "public, max-age=30"},
		{"/v1/assets", "public, max-age=30"},
		{"/v1/assets/native", "public, max-age=30"},
		{"/v1/status", "public, max-age=10"},
		{"/v1/operations", "public, max-age=10"},
		{"/v1/contracts", "public, max-age=10"},
		{"/v1/contracts/stats", "public, max-age=10"},
		{"/v1/history", "public, max-age=60"},
		{"/v1/ohlc", "public, max-age=60"},
		{"/v1/vwap", "public, max-age=30"},
		{"/v1/twap", "public, max-age=30"},
		{"/v1/markets", "public, max-age=60"},
		{"/v1/pairs", "public, max-age=60"},
		{"/v1/sources", "public, max-age=60"},
		// /v1/aggregators is on the 30s/60s short band.
		{"/v1/aggregators", "public, max-age=30"},
		{"/v1/network/stats", "public, max-age=10"},
		{"/v1/oracle/lastprice", "public, max-age=30"},
		// Non-cacheable directives unchanged.
		{"/v1/healthz", "no-store"},
		{"/v1/account/me", "private, no-store"},
		{"/v1/auth/sep10/challenge", "private, no-store"},
		{"/v1/price/tip", "private, no-cache, must-revalidate"},
		{"/v1/observations", "private, no-cache, must-revalidate"},
		{"/v1/something-new", "private, no-store"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := policyForPath(tc.path, false); got != tc.want {
				t.Errorf("policyForPath(%q, cdn=false) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestCacheControlWithCDN_FalseDropsSMaxAge confirms the middleware
// constructor honours the cdnEnabled flag end-to-end (handler-side
// header observation, not just policyForPath).
func TestCacheControlWithCDN_FalseDropsSMaxAge(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := CacheControlWithCDN(false)(inner)

	req := httptest.NewRequest(http.MethodGet, "/v1/markets", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control with cdn=false = %q, want \"public, max-age=60\"", got)
	}
}

// TestPolicyForPath_OracleLatestIsNotTheOraclePrefixBand pins the
// separation explicitly, because it is the kind of thing a later edit
// "tidies" back together. /v1/oracle/latest is a LATEST-OBSERVATION-per-
// source surface: no closed-bucket contract, no staleness flag on the
// reading. It sat in the 300s catalogue band solely because it matched
// the `/v1/oracle/` prefix arm — five minutes of shared-cache lag on a
// surface whose whole claim is "latest". Its siblings genuinely are
// catalogue reads and must stay where they are.
func TestPolicyForPath_OracleLatestIsNotTheOraclePrefixBand(t *testing.T) {
	latest := policyForPath("/v1/oracle/latest", true)
	prefix := policyForPath("/v1/oracle/streams", true)
	if latest == prefix {
		t.Fatalf("/v1/oracle/latest fell back into the /v1/oracle/ prefix band (%q) — it must carry the short closed-bucket band", latest)
	}
	if latest != "public, max-age=30, s-maxage=5" {
		t.Errorf("/v1/oracle/latest = %q, want the 5s shared band", latest)
	}
	if !strings.Contains(prefix, "s-maxage=300") {
		t.Errorf("/v1/oracle/streams = %q, want the 300s catalogue band (only `latest` was moved)", prefix)
	}
}

// TestPolicyForPath_OracleSEP40PassthroughsShareTheShortBand pins that
// /v1/oracle/lastprice and /v1/oracle/x_last_price are, like
// /v1/oracle/latest, "last observed price" surfaces with no closed-bucket
// contract, and /v1/oracle/prices is itself closed-bucket (it excludes the
// in-progress bucket, same as /v1/price/changes). None of the three belong
// in the `/v1/oracle/` prefix's 300s catalogue band, yet only
// /v1/oracle/latest had been carved out of it — the other three fell
// through and served a stale 5-minute-lag directive on a "latest" surface.
func TestPolicyForPath_OracleSEP40PassthroughsShareTheShortBand(t *testing.T) {
	const wantShortBand = "public, max-age=30, s-maxage=5"
	for _, path := range []string{"/v1/oracle/lastprice", "/v1/oracle/prices", "/v1/oracle/x_last_price"} {
		got := policyForPath(path, true)
		if got != wantShortBand {
			t.Errorf("policyForPath(%q) = %q, want the short band %q shared with /v1/oracle/latest", path, got, wantShortBand)
		}
		if strings.Contains(got, "s-maxage=300") {
			t.Errorf("policyForPath(%q) = %q — still in the 300s /v1/oracle/ prefix catalogue band", path, got)
		}
	}
	// A genuine catalogue sibling must stay in the 300s band — this guards
	// against over-widening the exact-match carve-out into a new prefix.
	if prefix := policyForPath("/v1/oracle/streams", true); !strings.Contains(prefix, "s-maxage=300") {
		t.Errorf("/v1/oracle/streams = %q, want the 300s catalogue band (only the four named SEP-40 passthroughs moved)", prefix)
	}
}

// TestPolicyForPath_PriceSharedTTLIsBoundedByTheProbe encodes WHY the
// number is 5 and not 60: the SLA probe's closed-bucket freshness target
// is 150s and is built from 60s bucket + 30s CAGG end_offset + <=30s
// schedule + runtime. A shared TTL adds itself to that worst case, so any
// s-maxage above ~30s puts a compliant origin outside its own SLA at the
// edge. The test fails if someone raises it back.
func TestPolicyForPath_PriceSharedTTLIsBoundedByTheProbe(t *testing.T) {
	const probeBudgetSeconds = 30 // headroom inside the 150s target
	for _, path := range SLOPriceRoutes {
		got := policyForPath(path, true)
		m := regexp.MustCompile(`s-maxage=(\d+)`).FindStringSubmatch(got)
		if m == nil {
			t.Errorf("%s = %q, expected an s-maxage directive when the CDN is enabled", path, got)
			continue
		}
		secs, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("%s: unparseable s-maxage in %q: %v", path, got, err)
		}
		if secs > probeBudgetSeconds {
			t.Errorf("%s has s-maxage=%d — above the %ds headroom inside the SLA probe's 150s closed-bucket target (#344)", path, secs, probeBudgetSeconds)
		}
	}
}

// TestPolicyForPath_OperationsSharesTheLedgerListBand pins the
// adjudication and the reason for it, because "operations" reads like a
// catalogue and a later tidy-up would file it next to /v1/markets.
//
// /v1/operations shipped `private, no-store` — not a decision, just an
// unclassified route landing on the conservative default. It is the sibling
// of /v1/ledgers: a network-wide directory advancing once per ledger (~5s),
// fronted by its own server-side cache. So it takes the SAME band.
//
// It must NOT take the 300s catalogue band. explorer.opsDirCache serves stale
// on expiry and only refreshes ON a request, so at a low arrival rate an
// entry's age is bounded by the inter-arrival gap, not by its 10s TTL
// (measured on r1: `as_of` 93.2s behind after a quiet window). A
// 300s shared cache would compound that real staleness rather than absorb a
// burst.
func TestPolicyForPath_OperationsSharesTheLedgerListBand(t *testing.T) {
	ops := policyForPath("/v1/operations", true)
	if ops != "public, max-age=10, s-maxage=15" {
		t.Fatalf("/v1/operations = %q, want the status-like short band it shares with /v1/ledgers", ops)
	}
	if ledgers := policyForPath("/v1/ledgers", true); ops != ledgers {
		t.Errorf("/v1/operations = %q but its sibling /v1/ledgers = %q — the two listings must not drift apart", ops, ledgers)
	}
	if strings.Contains(ops, "s-maxage=300") {
		t.Errorf("/v1/operations = %q — the 300s catalogue band would pin an already-stale directory at the edge", ops)
	}
	// Exact match, not a prefix: nothing else hangs off /v1/operations, and a
	// future sub-route must be adjudicated on its own merits rather than
	// inherit this band by accident.
	if sub := policyForPath("/v1/operations/something", true); sub != "private, no-store" {
		t.Errorf("policyForPath(%q) = %q, want the conservative default (the case is an exact match by design)", "/v1/operations/something", sub)
	}
}

// TestCacheControl_OperationsBandIgnoresTheQuery pins that /v1/operations is
// banded on its path alone: the handler refuses every ?ledger= form with a
// no-store problem, so no query value may lift the page into the
// closed-ledger band.
func TestCacheControl_OperationsBandIgnoresTheQuery(t *testing.T) {
	cases := []struct {
		target string
		cdn    bool
		want   string
	}{
		{"/v1/operations?ledger=64000000", true, "public, max-age=10, s-maxage=15"},
		{"/v1/operations?ledger=64000000&limit=2000", false, "public, max-age=10"},
		{"/v1/operations", true, "public, max-age=10, s-maxage=15"},
		{"/v1/operations?ledger=", true, "public, max-age=10, s-maxage=15"},
		{"/v1/operations?ledger=0", true, "public, max-age=10, s-maxage=15"},
		{"/v1/operations?cursor=63000000.4.7", true, "public, max-age=10, s-maxage=15"},
		{"/v1/operations?ledger=abc", true, "public, max-age=10, s-maxage=15"},
		{"/v1/operations?ledger=4294967296", true, "public, max-age=10, s-maxage=15"},
		{"/v1/ledgers?ledger=64000000", true, "public, max-age=10, s-maxage=15"},
	}
	for _, tc := range cases {
		mw := CacheControlWithCDN(tc.cdn)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))
		if got := rec.Header().Get("Cache-Control"); got != tc.want {
			t.Errorf("GET %s (cdn=%v) Cache-Control = %q, want %q", tc.target, tc.cdn, got, tc.want)
		}
	}
}

// TestPolicyForPath_PublicRoutesOffTheDefault pins the public GET routes
// that would otherwise reach the conservative default with nobody having chosen
// it. Each assertion is the band its data supports; the live-state trio
// stays private, no-store but through an explicit arm (the registry test
// below is what proves the arm exists).
func TestPolicyForPath_PublicRoutesOffTheDefault(t *testing.T) {
	const (
		short     = "public, max-age=10, s-maxage=15"
		current   = "public, max-age=30, s-maxage=60"
		catalogue = "public, max-age=60, s-maxage=300"
		private   = "private, no-store"
	)
	cid := "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	tests := []struct{ path, want string }{
		{"/v1/contracts/" + cid, short},
		{"/v1/contracts/" + cid + "/interactions", short},
		{"/v1/contracts/" + cid + "/code-history", short},
		// Latest-N listing with no cursor: tip-advancing, not closed history.
		{"/v1/contracts/" + cid + "/transfers", short},
		{"/v1/status/notices", short},
		{"/v1/lending/pools/" + cid + "/reserves", current},
		{"/v1/external/assets", current},
		{"/v1/external/assets/bitcoin", current},
		{"/v1/markets/sources", catalogue},
		{"/v1/mev", catalogue},
		{"/v1/search", catalogue},
		// Live freeze state: must never be edge-cached.
		{"/v1/anomalies", private},
		{"/v1/divergence", private},
		{"/v1/divergence/series", private},
		// Not over-matched by the new patterns.
		{"/v1/contracts/" + cid + "/transfers/x", private},
		{"/v1/lending/pools/" + cid + "/reserves/x", private},
	}
	for _, tc := range tests {
		if got := policyForPath(tc.path, true); got != tc.want {
			t.Errorf("policyForPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
	for _, p := range []string{"/v1/anomalies", "/v1/divergence", "/v1/divergence/series"} {
		if !classified(p) {
			t.Errorf("%s reaches the default arm; its no-store must be an explicit decision", p)
		}
	}
}

// TestPolicyForPath_EveryRegisteredGETRouteIsAdjudicated walks the GET
// patterns the v1 server and its sub-packages register and fails on any
// that reaches the default arm without an allowlist entry, and on any
// allowlist entry that does not. A new public route therefore cannot
// silently inherit `private, no-store`.
func TestPolicyForPath_EveryRegisteredGETRouteIsAdjudicated(t *testing.T) {
	patterns := registeredGETPatterns(t)
	if len(patterns) < 50 {
		t.Fatalf("found only %d GET routes — the source scan is broken, and a "+
			"guard that finds nothing to check passes forever", len(patterns))
	}
	seen := map[string]bool{}
	for _, pat := range patterns {
		seen[pat] = true
		path := samplePath(pat)
		_, allowed := defaultPolicyAllowlist[pat]
		switch {
		case !classified(path) && !allowed:
			t.Errorf("GET %s (probed as %s) reaches policyForPath's default arm: give it an explicit arm, or add it to defaultPolicyAllowlist with a reason", pat, path)
		case classified(path) && allowed:
			t.Errorf("GET %s is in defaultPolicyAllowlist but now matches an explicit arm — remove the stale entry", pat)
		}
	}
	for pat := range defaultPolicyAllowlist {
		if !seen[pat] {
			t.Errorf("defaultPolicyAllowlist names %s, which is no longer a registered GET route", pat)
		}
	}
}
