package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
)

// Envelope is the shape of every 2xx JSON response. See
// docs/reference/api-design.md §4.
type Envelope struct {
	Data any       `json:"data"`
	AsOf time.Time `json:"as_of"`
	// CoverageFrom is the earliest instant this deployment holds
	// served-tier price history at for the pair the request named — the
	// bottom of the range the response's emptiness can speak for.
	//
	// It lives on the ENVELOPE rather than inside `data` because the
	// surfaces that need it return three different body shapes (a bar
	// series, a point series, a bare trade array) and one of them has no
	// object to hang it on. Present only on the surfaces that probe it
	// (/v1/ohlc, /v1/history, /v1/chart, /v1/price/at) and only when the
	// probe reached an answer; absent means UNKNOWN, never "from the
	// beginning of time".
	CoverageFrom *time.Time  `json:"coverage_from,omitempty"`
	Sources      []string    `json:"sources,omitempty"`
	Flags        Flags       `json:"flags"`
	Pagination   *Pagination `json:"pagination,omitempty"`
}

// Flags are the advisory quality markers per HA plan §9.
//
// Field semantics:
//
//   - Stale: response is below this surface's documented baseline
//     contract — e.g. on /v1/price the closed-bucket VWAP wasn't
//     available so we degraded to last-trade. NOT used on
//     /v1/price/tip's last-good-price fallback (that's in-contract;
//     see ADR-0018 §"flags.stale semantic").
//   - ReducedRedundancy: cross-region redundancy is degraded —
//     R2/R3 set this when R1's last successful completeness run is
//     stale per ADR-0017.
//   - Triangulated: rate was computed via chain-pricing through a
//     pivot (typically USD), not from a directly-traded pair.
//   - DivergenceWarning: anomaly-detection or cross-reference
//     observed a meaningful divergence; consumers should treat the
//     value with caution. Fires per ADR-0019 anomaly.ActionWarn AND
//     per future internal/divergence/ cross-reference checks.
//   - Frozen: anomaly detection refused to publish the new bucket;
//     this response carries the previous bucket's last-known-good
//     value (ADR-0019 freeze policy). Only fires on /v1/price; the
//     tip + observations surfaces ignore freeze.
//   - OutsideCoverage: the requested time range ends at or before the
//     pair's `coverage_from` — every instant asked for predates the
//     history this deployment holds, so the empty answer is a coverage
//     statement, not a market one. Only set on the four windowed
//     surfaces, only when the floor is KNOWN, and never on a range that
//     straddles the floor (that range contains covered time).
//   - SingleSource: the bucket had only one contributing source.
//     Informational; combined with Frozen this is the manipulation
//     signature.
//   - Diverged: a TRIANGULATED composite came from routes that
//     DISAGREED (the router's divergence signal). Only meaningful on
//     the /v1/price triangulated serve path; omitted when false.
//   - Rerouted: a TRIANGULATED composite SUBSTITUTED around a dry
//     configured chain leg — the router walked an alternative path
//     rather than the documented direct chain (R3). Only meaningful on
//     the /v1/price triangulated serve path; omitted when false.
type Flags struct {
	Stale             bool `json:"stale"`
	ReducedRedundancy bool `json:"reduced_redundancy"`
	Triangulated      bool `json:"triangulated"`
	DivergenceWarning bool `json:"divergence_warning"`
	// DivergenceChecked is true only when a live cross-reference check ran
	// (≥ `min_sources_for_warning` responding references — the SAME quorum
	// the worker gates its verdict on; below it the cross-check reaches no
	// verdict at all). When false, `divergence_warning` is NOT
	// meaningful — the check is blind (references dark, or no record yet), so
	// a `false` warning must not be read as "prices agree" (CS-087).
	//
	// Set on the surfaces that consult the verdict: /v1/price, its
	// ?window= variant, /v1/price/tip, /v1/price/tip/stream and /v1/vwap —
	// each looking it up by BASE across every canonical spelling of the
	// asset (lookupDivergenceFlag). On every other envelope that carries
	// Flags the field is false and means "not consulted on this
	// surface", never "checked and clean": /v1/price/batch, /v1/twap,
	// the SEP-40 passthroughs, /v1/observations and its stream all
	// serve values the looker is never asked about. The observations
	// pair is the deliberate case — raw per-source rows carry no
	// aggregated value for a base-level verdict to vouch for (see
	// handleObservations).
	DivergenceChecked bool `json:"divergence_checked"`
	// OutsideCoverage marks an empty answer whose requested range ends
	// at or before the envelope's `coverage_from` — the window predates
	// this deployment's history for the pair, so nothing was there to
	// return. Without it an empty series and a quiet market are the same
	// bytes. omitempty hides it when false, which includes every request
	// whose coverage floor could not be established.
	OutsideCoverage bool `json:"outside_coverage,omitempty"`
	Frozen          bool `json:"frozen,omitempty"`
	SingleSource    bool `json:"single_source,omitempty"`
	// Diverged marks a triangulated composite whose contributing routes
	// disagreed (the aggregator's router divergence signal, persisted to
	// cachekeys.VWAPCompositeMeta). Surfaced on the /v1/price
	// triangulated serve path only; omitempty hides it when false.
	Diverged bool `json:"diverged,omitempty"`
	// Rerouted marks a triangulated composite that substituted around a
	// DRY configured chain leg — the price came via an alternative path,
	// not the documented direct chain (R3). Surfaced on the /v1/price
	// triangulated serve path only; omitempty hides it when false.
	Rerouted bool `json:"rerouted,omitempty"`
	// UnverifiedTickerCollision fires on `/v1/assets/{id}` when the
	// requested asset's code matches a verified currency's Stellar
	// ticker but its issuer doesn't match the verified entry — i.e.
	// someone issued their own "USDC" on Stellar. The matching
	// `unverified_warning` payload on the AssetDetail body carries
	// the pointer to the verified asset. See R-018 /
	// docs/architecture/multi-network-assets-migration.md.
	UnverifiedTickerCollision bool `json:"unverified_ticker_collision,omitempty"`
	// FiltersIgnored names the row-narrowing query parameters the
	// response did NOT apply, spelled as the caller sent them
	// ("type", "code", "issuer", "q"). Empty — and omitted — when
	// everything supplied was applied.
	//
	// A listing that DROPPED a filter and one that genuinely matched
	// on it are otherwise the same 200 over the same wire shape, so a
	// client re-filtering the page, or a person reading a search
	// result, has nothing to key on but the spec's prose. `/v1/assets`
	// sets it where the rows come from a source that cannot narrow:
	// the class-scoped catalogue listings
	// (`asset_class=fiat|stablecoin|crypto`) and the lean AssetReader
	// fallback.
	FiltersIgnored []string `json:"filters_ignored,omitempty"`
}

// Pagination is present on list-returning endpoints only.
type Pagination struct {
	Next string `json:"next,omitempty"`
}

// Problem is the RFC 9457 error payload. Custom fields are
// snake_case; `Instance` is typically the request URL.
//
// RequestID is an extension field per RFC 9457 §3.2 (unknown
// members allowed). It echoes the X-Request-ID header so clients
// can correlate a failure they saw with server logs without
// parsing headers separately — and so bug reports that include
// the body are sufficient for support to find the trace.
// CoverageFrom / OutsideCoverage are the same two extension members the
// 2xx envelope carries, on the one windowed surface whose empty answer
// is an ERROR rather than an empty body: /v1/price/at answers "no
// closed bucket at that instant" with a 404, which is the identical
// ambiguity — dead market or before the history held — in
// problem+json clothing. RFC 9457 §3.2 admits unknown members, so they ride here
// rather than forcing that endpoint's contract to 200.
type Problem struct {
	Type            string     `json:"type"`
	Title           string     `json:"title"`
	Status          int        `json:"status"`
	Detail          string     `json:"detail,omitempty"`
	Instance        string     `json:"instance,omitempty"`
	RequestID       string     `json:"request_id,omitempty"`
	CoverageFrom    *time.Time `json:"coverage_from,omitempty"`
	OutsideCoverage bool       `json:"outside_coverage,omitempty"`
}

// writeJSON writes the Envelope + 200. The convention everywhere in
// v1 handlers.
func writeJSON(w http.ResponseWriter, data any, flags Flags, sources ...string) {
	writeEnvelope(w, Envelope{
		Data:    data,
		AsOf:    time.Now().UTC(),
		Sources: sources,
		Flags:   flags,
	})
}

// writeJSONCoverage is [writeJSON] plus the coverage-floor annotation
// the empty-window surfaces attach. coverageFrom nil (the probe reached
// no answer) leaves the field off the wire entirely.
func writeJSONCoverage(w http.ResponseWriter, data any, flags Flags, coverageFrom *time.Time) {
	writeEnvelope(w, Envelope{
		Data:         data,
		AsOf:         time.Now().UTC(),
		CoverageFrom: coverageFrom,
		Flags:        flags,
	})
}

// writeEnvelope writes a pre-constructed Envelope. Used by handlers
// that need to set Pagination or other fields writeJSON doesn't
// accept as params.
func writeEnvelope(w http.ResponseWriter, env Envelope) {
	writeEnvelopeStatus(w, http.StatusOK, env)
}

// writeEnvelopeStatus writes a pre-constructed Envelope with an
// explicit 2xx status code. Used by handlers whose public contract
// is not plain 200 OK.
func writeEnvelopeStatus(w http.ResponseWriter, status int, env Envelope) {
	if env.AsOf.IsZero() {
		env.AsOf = time.Now().UTC()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

// writeProblem writes an RFC 9457 error response. Handlers call
// this instead of http.Error to keep the wire contract consistent.
//
// typeURL is the stable error-type URL (document the taxonomy at
// https://api.stellarindex.io/errors/<name>); title is a short
// human headline; status is the HTTP code; detail is the freeform
// per-request message (optional).
//
// A 500 whose request deadline has ALREADY expired is rewritten to the
// canonical retryable timeout problem — see requestDeadlineExpired. That
// covers the BLANKET middleware deadline only; an error path holding the
// error from a handler's OWN budget must call writeProblemErr instead.
func writeProblem(w http.ResponseWriter, r *http.Request, typeURL, title string, status int, detail string) {
	writeProblemCoverage(w, r, typeURL, title, status, detail, nil, false)
}

// writeProblemCoverage is [writeProblem] carrying the coverage-floor
// extension members. Only /v1/price/at uses it — the one windowed
// surface whose "nothing there" answer is a 404 body rather than an
// empty array. Every rewrite rule writeProblem applies (deadline →
// retryable 503, no-store, the 401 challenge) applies here identically,
// which is why this is the shared body and writeProblem the thin call.
func writeProblemCoverage(
	w http.ResponseWriter, r *http.Request,
	typeURL, title string, status int, detail string,
	coverageFrom *time.Time, outsideCoverage bool,
) {
	if status == http.StatusInternalServerError && requestDeadlineExpired(r) {
		typeURL, title, status, detail = requestTimeoutType, requestTimeoutTitle,
			http.StatusServiceUnavailable, requestTimeoutDetail
	}
	p := Problem{
		Type:            typeURL,
		Title:           title,
		Status:          status,
		Detail:          detail,
		Instance:        r.URL.RequestURI(),
		RequestID:       middleware.RequestIDFrom(r),
		CoverageFrom:    coverageFrom,
		OutsideCoverage: outsideCoverage,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	// Errors override the cache-control middleware's per-route
	// directive: never cache an error. Otherwise a CDN serving
	// /v1/coins (which the middleware tags `public, max-age=60,
	// s-maxage=300`) would cache a transient 400/404/500 for the
	// next 5 minutes and replay it to other anonymous clients on
	// the same cache key.
	w.Header().Set("Cache-Control", "no-store")
	// RFC 7235 §3.1: every 401 response MUST include a
	// WWW-Authenticate header naming at least one challenge the
	// client can use. Pre-fix our 401s emitted the problem+json
	// envelope but no WWW-Authenticate, leaving programmatic
	// clients without a way to discover the accepted scheme. Our
	// authenticated endpoints all accept Bearer (API key + SEP-10
	// token); the magic-link cookie path is parallel and doesn't
	// have a standard challenge token, so we advertise Bearer only.
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="stellarindex.io"`)
	}
	// Keyed on the TYPE, not the status: writeProblemErr rewrites its own
	// upgrade before calling in, so by the time it reaches here the status
	// is already 503 and only the type still identifies the condition.
	// Both upgrade legs therefore carry the hint.
	if typeURL == requestTimeoutType {
		w.Header().Set("Retry-After", retryAfterRequestTimeout)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// The canonical problem for "this server ran out of its request budget
// before the handler could answer" — the wire shape writeProblem
// substitutes for a 500 raised after the request deadline expired.
const (
	requestTimeoutType  = "https://api.stellarindex.io/errors/request-timeout"
	requestTimeoutTitle = "Request timed out"
	// No budget figure in the detail: the effective bound is the
	// deployment's api.request_timeout, not a constant this package can
	// quote truthfully.
	requestTimeoutDetail = "the request exceeded this server's request budget before " +
		"the handler could answer; retry shortly."
	// A 503 whose body says "retry shortly" but carries no Retry-After
	// leaves every client to guess, and the guess is "immediately" —
	// precisely the retry storm a server that just ran out of budget must
	// not receive. 5s is the in-process busy-ness value the rest of the
	// API already uses (the explorer's retryAfterBusy, writeChartTimeout);
	// the 30s figure is reserved for a dependency outage
	// (writeCacheUnavailableProblem), which a blown request budget is not.
	retryAfterRequestTimeout = "5"
)

// requestDeadlineExpired reports whether the blanket
// middleware.RequestTimeout deadline on r.Context() has already fired.
//
// It exists so writeProblem can upgrade a 500 to a retryable 503 in ONE
// place. Roughly fifty handler error paths reach writeProblem with
// StatusInternalServerError and no timeout branch of their own, and the
// ones that hand r.Context() STRAIGHT to a store — /v1/anomalies, the
// assets + price families, /v1/auth/sep10 — have no per-call context for
// handlerTimedOut to inspect, so there is nothing for a per-site fix to
// key on. A deadline is retryable capacity, not an internal fault: that
// is the rule the repo already writes down (writeLendingReservesTimeout,
// explorer writeReadTimeout) and the rule the sla-probe's
// availability_pct is scored against, where a 500 permanently books a
// failure a retry would have cleared.
//
// It keys on r.Context() specifically, so a tighter per-handler budget
// still reaches its own more specific `…-timeout` branch first; only the
// blanket deadline lands here. And only 500 is rewritten — a 400/404
// decided on the request's own merits stays the client's answer even if
// the deadline blew while it was being written.
//
// LIMIT, and why writeProblemErr exists: a handler that caps its own read
// with context.WithTimeout(r.Context(), 8s) under a 15s global blows the
// INNER budget first, and r.Context() is still alive when it does — so
// this predicate is false and the deadline books a 500. The inner budget
// is the dominant shape (every request-derived budget in this package is
// 3-12s), so the blanket-deadline check alone closes the smaller half of
// the class.
//
// The trade-off, stated: a genuine internal fault that happens to be
// reported after the deadline expired is relabelled a timeout. The
// handler's own ERROR log line still carries the real error, so
// diagnosis is unaffected, and once the budget is gone "retry" is the
// only advice the caller can act on anyway.
func requestDeadlineExpired(r *http.Request) bool {
	return errors.Is(r.Context().Err(), context.DeadlineExceeded)
}

// writeProblemErr is writeProblem for a call site that has the failing
// error in hand. It rewrites a FAULT status to the same retryable
// request-timeout 503 when the ERROR is a deadline — the case
// requestDeadlineExpired structurally cannot see, because a handler's own
// context.WithTimeout(r.Context(), …) budget expires while r.Context()
// still has budget left. The store returns context.DeadlineExceeded, the
// site has no timeout branch, and the request books `errors/internal`
// 500: "we broke", for a condition a retry clears.
//
// Both fault statuses are rewritten. 500 attributes the failure to this
// server and 502 to its upstream, and a deadline is neither. The supply
// endpoint is the 502 case: an 8s ceiling on a lake read rendered as
// "Supply read failed", which sends the reader looking at ClickHouse for
// a bound this process imposed. A 4xx is decided on the request's own
// merits and stays the client's answer; an already-retryable 503 keeps
// its own more specific type.
//
// Deliberately keyed on errors.Is over the error rather than on a
// context: a driver that RE-PHRASES the cancellation instead of wrapping
// it (Postgres SQLSTATE 57014) is not caught here, and that case is what
// handlerTimedOut and its per-call context are for. A site holding a
// per-call ctx and a named timeout type should branch on handlerTimedOut
// and keep its more specific `…-timeout` shape; this is the fallback for
// the sites that have neither.
func writeProblemErr(
	w http.ResponseWriter, r *http.Request, err error,
	typeURL, title string, status int, detail string,
) {
	faultStatus := status == http.StatusInternalServerError || status == http.StatusBadGateway
	if faultStatus && errors.Is(err, context.DeadlineExceeded) {
		typeURL, title, status, detail = requestTimeoutType, requestTimeoutTitle,
			http.StatusServiceUnavailable, requestTimeoutDetail
	}
	writeProblem(w, r, typeURL, title, status, detail)
}

// clientAborted reports whether a reader-returned error came from
// the client cancelling its request. When true, handlers SHOULD
// return without writing a response — the client has already gone,
// and the obs.HTTPMetrics middleware will then label the request
// 499 (NGINX-style "client closed request") rather than the
// misleading 500 a writeProblem would produce.
//
// Decision rule: the request context must be done AND its cause must
// be CANCELLATION. net/http cancels r.Context() with
// [context.Canceled] when the peer hangs up, so that is the one state
// in which nobody is left to read a response.
//
// A done request context whose Err is [context.DeadlineExceeded] is
// the opposite case — a SERVER-side budget expiring with the client
// still on the wire. Two of those exist: the cold-path
// context.WithTimeout guards inside handlers (#1082, #1099-#1105), and
// since C3-102 the blanket middleware.RequestTimeout deadline, which
// wraps r.Context() itself. Testing only `Err() != nil` conflated the
// second with a client abort: on the global deadline the handler
// returned silently and net/http emitted a BODYLESS 200, which reads
// to a client as an authoritative empty result rather than a failure
// (a Blend pool with real supply rendered as "0 reserves / $0 TVL").
// Both server-side deadlines belong on the 503 problem+json path.
//
// The 499 relabel above does NOT cover that case, which is why the
// bodyless 200 was invisible: obs.HTTPMetrics is installed OUTSIDE
// middleware.RequestTimeout (server.go's Handler stack), so the
// r.Context() it inspects is the UN-deadlined one. On a server-side
// deadline with the peer still connected that context's Err() is nil,
// the 499 override never fires, and the recorder's default
// http.StatusOK stands — so those requests were counted as 200 and, on
// that status, admitted into http_request_success_duration_seconds, the
// latency SLO's success numerator. They are 5xx now.
//
// Handlers should structure error handling as:
//
//	if err != nil {
//	    if clientAborted(r, err) { return }
//	    if handlerTimedOut(callCtx, err) {
//	        // 503 timeout response
//	    }
//	    // 500 internal
//	}
//
// `err` is unused for the abort decision but kept in the signature
// because it's the natural call site (handlers always have it) and
// keeps the call sites stable.
func clientAborted(r *http.Request, _ error) bool {
	return errors.Is(r.Context().Err(), context.Canceled)
}

// handlerTimedOut reports whether a handler-scoped context (created
// via context.WithTimeout to cap an individual storage call) hit
// its deadline. Use this on the per-call context — NOT
// r.Context() — so genuine deadline-exceeded paths are recognised
// even when the upstream driver returns its own
// statement-cancellation error rather than wrapping
// context.DeadlineExceeded.
//
// Background: the Postgres driver propagates a Go context cancellation
// to PostgreSQL via the v3 cancel-request protocol, then returns the
// resulting `canceling statement due to user request` (SQLSTATE
// 57014) — which does NOT unwrap to [context.DeadlineExceeded].
// `errors.Is(err, context.DeadlineExceeded)` therefore misses every
// case where a per-call deadline fired and the driver beat the
// caller to noticing. The cleanest signal is the per-call context
// itself: if its Err() is DeadlineExceeded, the request DID time
// out regardless of how the driver phrased the resulting error.
//
// The OR with errors.Is keeps drivers that DO wrap correctly
// (Timescale's hypercore extension does in some paths) on the same
// branch.
//
// R-021 in `docs/review-2026-05-10.md` — pre-fix, /v1/markets cold
// cache returned `500 Internal error` instead of `503 markets-timeout`.
func handlerTimedOut(callCtx context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return callCtx.Err() == context.DeadlineExceeded
}

// transientStorageErr reports whether a storage-layer error looks
// like a transient infrastructure hiccup rather than a real server
// bug. Handlers SHOULD map true to a 503 problem+json (retryable)
// rather than the misleading 500 a writeProblem default would
// produce; the sla-probe's availability_pct counts 5xx as failure,
// so a single transient driver-level error costs an availability
// point even when the underlying request was processable on a
// retry.
//
// Examples this catches:
//
//   - **SQLSTATE 57014 NOT carried by a context cancellation.**
//     Postgres can issue `canceling statement due to user request`
//     from server-side statement_timeout, lock_timeout, or
//     idle_in_transaction_session_timeout — none of which trip
//     [clientAborted] (the http request context is alive) or
//     [handlerTimedOut] (the per-call context hasn't deadlined).
//     The result reaches the handler as a bare 57014; without this
//     helper it returns 500.
//   - **driver-bad-conn errors.** `driver: bad connection`
//     surfaces when a Postgres backend was killed (admin restart,
//     OOM killer, idle-connection reaper) between checkout and
//     query execution. The connection-pool retry would normally
//     paper over it; surfaces only when retries are exhausted.
//   - **EOF / broken pipe.** Network-level transient between the
//     api binary and postgres or redis. Re-running the same
//     request would typically succeed.
//
// The classifier is INTENTIONALLY string-based for the SQLSTATE
// match — pgconn's typed `*pgconn.PgError.Code` would require importing
// the driver into the handler layer (already a dep, but a wider
// surface than strict). The substring `57014` is stable (it's
// wire-format from postgres itself, and pgx renders it into the
// error string as `(SQLSTATE 57014)`); the 'canceling statement'
// fragment is the human-readable companion the driver always includes.
//
// Caller pattern (mirrors clientAborted / handlerTimedOut order):
//
//	if err != nil {
//	    if clientAborted(r, err) { return }
//	    if handlerTimedOut(callCtx, err) { /* 503 timeout */ }
//	    if transientStorageErr(err) { /* 503 retry-later */ }
//	    /* 500 internal */
//	}
//
// Refs: #34 residual ("/v1/issuers returns HTTP 500 (fast ~50ms)
// on the sla-probe's request shape — real bug, low severity").
func transientStorageErr(err error) bool {
	if err == nil {
		return false
	}
	// Postgres UNREACHABLE, not merely slow (#371 F8). Every substring
	// arm below describes a connection that EXISTED and then misbehaved;
	// none of them matches pgx's failure to establish one in the first
	// place, which is what a restarting/downed/failed-over Postgres
	// actually produces:
	//
	//	failed to connect to `host=127.0.0.1 user=si database=si`:
	//	  dial error (dial tcp 127.0.0.1:5432: connect: connection refused)
	//
	// So the ONE dependency-outage shape most likely to hit every handler
	// at once was the one shape that fell through to a 500 "Internal
	// error" — an outage indistinguishable from a bug in the logs, in the
	// 5xx SLA probe, and in every alert built on them.
	if unreachableStorageErr(err) {
		return true
	}
	s := err.Error()
	// SQLSTATE 57014 from postgres-side cancellations (not the
	// client-side context cancellation flavour, which clientAborted
	// already handles).
	if strings.Contains(s, "57014") || strings.Contains(s, "canceling statement") {
		return true
	}
	// pgx stdlib + the standard database/sql driver-bad-connection
	// surface. Pool retry exhausted by this point.
	if strings.Contains(s, "driver: bad connection") || strings.Contains(s, "bad connection") {
		return true
	}
	// Network-level transients between the api and postgres / redis.
	if strings.Contains(s, "broken pipe") || strings.Contains(s, "connection reset") ||
		strings.Contains(s, "unexpected EOF") || strings.Contains(s, "EOF") {
		return true
	}
	return false
}

// unreachableStorageErr reports whether err is a failure to REACH the
// storage dependency — the dial was refused, the socket died, or the host
// stopped resolving. Structural first, substrings second:
//
//   - Structural (errors.As/Is) is exact and survives rewording. pgx
//     v5 wraps its dial failure in *pgconn.ConnectError, which Unwraps to
//     the *net.OpError, so errors.As reaches it through the chain.
//   - The substrings are the belt to that braces: a driver, a pool
//     wrapper or an aggregating multi-error is free to render the cause
//     into a string instead of preserving the chain, and pgx's own
//     "failed to connect to …: dial error (…)" text is stable enough to
//     match on. Matching both ways is what the sibling classifiers do
//     (IsCacheUnavailable in cache_errors.go pairs a *net.OpError check
//     with a MISCONF substring for exactly this reason).
//
// Kept in step with the ClickHouse-side lakeUnreachable
// (internal/api/v1/explorer/reader.go) — same rule, different dependency;
// that package can't import this one (v1.Server embeds its Handler).
func unreachableStorageErr(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "failed to connect") ||
		strings.Contains(s, "dial error") ||
		strings.Contains(s, "no such host")
}
