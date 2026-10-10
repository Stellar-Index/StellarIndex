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
	Data any      `json:"data"`
	AsOf WireTime `json:"as_of"`
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
	CoverageFrom *WireTime `json:"coverage_from,omitempty"`
	// Withheld names the requested ids a batch surface omitted from
	// `data` because a serving gate declined to publish their price —
	// distinct from an id omitted for having no data. Only
	// /v1/price/batch sets it; absent when nothing was withheld.
	Withheld []string `json:"withheld,omitempty"`
	// Thin names the requested ids /v1/price/batch served under
	// `include_thin=true` from a market below the substance floor.
	Thin       []string    `json:"thin,omitempty"`
	Sources    []string    `json:"sources,omitempty"`
	Flags      Flags       `json:"flags"`
	Pagination *Pagination `json:"pagination,omitempty"`
}

// Flags are the advisory quality markers per HA plan §9.
//
//   - Stale: below this surface's documented baseline contract (e.g. /v1/price
//     degraded to last-trade). NOT used on /v1/price/tip's last-good fallback
//     (ADR-0018 §"flags.stale semantic").
//   - ReducedRedundancy: cross-region redundancy degraded (ADR-0017).
//   - Triangulated: computed via a pivot (typically USD), not a directly-traded pair.
//   - DivergenceWarning: cross-reference or anomaly detection saw a meaningful
//     divergence (ADR-0019, internal/divergence); treat the value with caution.
//   - Frozen: anomaly detection refused the new bucket; the response carries the held
//     last-known-good value with its own observed_at, and Stale set (ADR-0019). On
//     /v1/price, /v1/price/batch and SEP-40 lastprice/x_last_price; tip +
//     observations ignore freeze. FrozenChecked separates "confirmed not frozen" from
//     "marker read failed" (as DivergenceChecked).
//   - OutsideCoverage: the range ends at or before the pair's `coverage_from`, so the
//     empty answer is a coverage statement. Only on the four windowed surfaces, only
//     when the floor is KNOWN, never on a range straddling it.
//   - SingleSource: one contributing source; with Frozen, the manipulation signature.
//   - Diverged / Rerouted / PivotUnverified: only on the /v1/price triangulated path,
//     omitted when false. Diverged: routes DISAGREED. Rerouted: SUBSTITUTED around a
//     dry configured leg. PivotUnverified: a leg priced only from par-valued stablecoin
//     prints, with no own-quote prints to check a de-peg against.
type Flags struct {
	Stale             bool `json:"stale"`
	ReducedRedundancy bool `json:"reduced_redundancy"`
	Triangulated      bool `json:"triangulated"`
	DivergenceWarning bool `json:"divergence_warning"`
	// DivergenceChecked is true only when a live cross-reference check ran
	// (>= `min_sources_for_warning` responding references, the SAME quorum the
	// worker gates its verdict on). When false the check is blind (references dark, or
	// no record yet), so a `false` warning must not be read as "prices agree"; a
	// `true` warning is then the last verdict carried forward through the outage.
	//
	// Set only on surfaces that consult the verdict: /v1/price, its ?window= variant,
	// /v1/price/tip and /v1/price/tip/stream, each asking for the exact (base, quote)
	// spelling served, never another alias's market (lookupDivergenceFlag). The
	// ?window= variant carries it only when the window is the one the verdict was
	// computed over (the aggregator's shortest). Elsewhere the field is false and means
	// "not consulted", never "checked and clean": /v1/price/at (a past bucket),
	// /v1/vwap (caller-chosen range from raw trades), /v1/price/batch, /v1/twap, the
	// SEP-40 passthroughs, and /v1/observations (raw per-source rows carry no
	// aggregated value for a base-level verdict to vouch for; see handleObservations).
	//
	// On /v1/price/tip/stream the lookup has its own short budget
	// ([tipStreamDivergenceBudget]); a verdict store too slow leaves the field false on
	// that event, so a false there can also mean "did not answer in time". The stream
	// degrades the flag, never the cadence.
	DivergenceChecked bool `json:"divergence_checked"`
	// OutsideCoverage marks an empty answer whose requested range ends
	// at or before the envelope's `coverage_from` — the window predates
	// this deployment's history for the pair, so nothing was there to
	// return. Without it an empty series and a quiet market are the same
	// bytes. omitempty hides it when false, which includes every request
	// whose coverage floor could not be established.
	OutsideCoverage bool `json:"outside_coverage,omitempty"`
	Frozen          bool `json:"frozen,omitempty"`
	// FrozenChecked is true only when the freeze marker was actually
	// read (looker wired and the read succeeded) — same two-valued
	// posture as DivergenceChecked. When false, `frozen` is NOT
	// meaningful: the check never ran, so `frozen: false` must not be
	// read as "confirmed not frozen".
	FrozenChecked bool `json:"frozen_checked,omitempty"`
	SingleSource  bool `json:"single_source,omitempty"`
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
	// PivotUnverified: a composite leg was all stablecoin prints at par, so a de-peg in it went unchecked.
	PivotUnverified bool `json:"pivot_unverified,omitempty"`
	// ThinMarket: the price was served under `include_thin=true` from a
	// market below the substance floor that would otherwise be withheld,
	// or by /v1/vwap or /v1/twap, which serve such a market by default.
	ThinMarket bool `json:"thin_market,omitempty"`
	// ProxyDeviation: a TRIANGULATED fiat:USD price rests on the assumption
	// that a declared USD peg is $1, and any declared peg's observed dollar
	// price is more than 2% from $1.
	ProxyDeviation bool `json:"proxy_deviation,omitempty"`
	// UnverifiedTickerCollision fires on `/v1/assets/{id}` when the
	// requested asset's code matches a verified currency's Stellar
	// ticker but its issuer doesn't match the verified entry — i.e.
	// someone issued their own "USDC" on Stellar. The matching
	// `unverified_warning` payload on the AssetDetail body carries
	// the pointer to the verified asset. See
	// docs/architecture/supply-pipeline.md#asset-identity.
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
	// Degraded marks a 200 whose body this process is carrying forward or
	// serving partially (a stale-while-revalidate entry past its TTL, a
	// last-good value after a failed refresh, a held frozen price, a dropped
	// best-effort section) — an answer the origin replaces once the fault
	// clears. Never on the wire: writeEnvelopeStatus turns it into
	// `Cache-Control: no-store`, so a shared cache cannot keep serving it
	// for its route's full band after recovery. It is set explicitly at
	// each such exit, never derived from Stale, which also covers fresh
	// reads of a lagging source that a cache may hold safely.
	Degraded bool `json:"-"`
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
	Type            string    `json:"type"`
	Title           string    `json:"title"`
	Status          int       `json:"status"`
	Detail          string    `json:"detail,omitempty"`
	Instance        string    `json:"instance,omitempty"`
	RequestID       string    `json:"request_id,omitempty"`
	CoverageFrom    *WireTime `json:"coverage_from,omitempty"`
	OutsideCoverage bool      `json:"outside_coverage,omitempty"`
	// Substance is the measurement behind a thin-market price-withheld
	// verdict; absent on every other problem.
	Substance *SubstanceEvidence `json:"substance,omitempty"`
	// Reason is the machine-readable [PriceWithheldReason] on a
	// price-withheld problem, the same enum SSE and asset rows carry.
	Reason PriceWithheldReason `json:"reason,omitempty"`
}

// writeJSON writes the Envelope + 200. The convention everywhere in
// v1 handlers.
func writeJSON(w http.ResponseWriter, data any, flags Flags, sources ...string) {
	writeEnvelope(w, Envelope{
		Data:    data,
		AsOf:    WireTime(time.Now().UTC()),
		Sources: sources,
		Flags:   flags,
	})
}

// dataVintage folds the fill times of the cached reads a response is built
// from into its as_of: the oldest fill when every read was cached, so an
// unchanged payload replays byte-identical (and its ETag 304s), else now.
type dataVintage struct {
	oldest time.Time
	live   bool
}

// note records one read's fill time; zero means an uncached, live read.
func (v *dataVintage) note(at time.Time) {
	if at.IsZero() {
		v.live = true
		return
	}
	if v.oldest.IsZero() || at.Before(v.oldest) {
		v.oldest = at
	}
}

func (v dataVintage) asOf() WireTime {
	if v.live || v.oldest.IsZero() {
		return WireTime(time.Now().UTC())
	}
	return WireTime(v.oldest.UTC())
}

// writeJSONCoverage is [writeJSON] plus the coverage-floor annotation
// the empty-window surfaces attach. coverageFrom nil (the probe reached
// no answer) leaves the field off the wire entirely.
func writeJSONCoverage(w http.ResponseWriter, data any, flags Flags, coverageFrom *time.Time) {
	writeEnvelope(w, Envelope{
		Data:         data,
		AsOf:         WireTime(time.Now().UTC()),
		CoverageFrom: wireTimePtr(coverageFrom),
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
		env.AsOf = WireTime(time.Now().UTC())
	}
	w.Header().Set("Content-Type", "application/json")
	if env.Flags.Degraded {
		w.Header().Set("Cache-Control", "no-store")
	}
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
	writeProblemCoverage(w, r, typeURL, title, status, detail, nil, false, nil, "")
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
	coverageFrom *time.Time, outsideCoverage bool, substance *SubstanceEvidence,
	reason PriceWithheldReason,
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
		CoverageFrom:    wireTimePtr(coverageFrom),
		OutsideCoverage: outsideCoverage,
		Substance:       substance,
		Reason:          reason,
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
	// client can use; without it programmatic clients have no way
	// to discover the accepted scheme. Our authenticated endpoints all
	// accept Bearer (API key + SEP-10 token); the magic-link cookie path
	// is parallel and doesn't have a standard challenge token, so we
	// advertise Bearer only.
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="stellarindex.io"`)
	}
	// Keyed on the TYPE, not the status: writeProblemErr rewrites its own
	// upgrade before calling in, so by the time it reaches here the status
	// is already 503 and only the type still identifies the condition.
	// Both upgrade legs therefore carry the hint.
	if typeURL == requestTimeoutType {
		middleware.MarkReadDeadline(r.Context())
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

// requestDeadlineExpired reports whether the blanket middleware.RequestTimeout
// deadline on r.Context() has already fired.
//
// It lets writeProblem upgrade a 500 to a retryable 503 in ONE place. About fifty
// handler error paths reach writeProblem with StatusInternalServerError and no
// timeout branch, and those that hand r.Context() straight to a store (/v1/anomalies,
// assets + price families, /v1/auth/sep10) have no per-call context for
// handlerTimedOut to inspect. A deadline is retryable capacity, not an internal
// fault (the rule in writeLendingReservesTimeout and explorer writeReadTimeout, and
// what the sla-probe's availability_pct is scored against: a 500 books a failure a
// retry would clear).
//
// It keys on r.Context(), so a tighter per-handler budget still reaches its own
// `...-timeout` branch first. Only 500 is rewritten; a 400/404 decided on the
// request's merits stays the client's answer.
//
// LIMIT, and why writeProblemErr exists: a handler capping its read with
// context.WithTimeout(r.Context(), 8s) under a 15s global blows the INNER budget
// first while r.Context() is still alive, so this is false and the deadline books a
// 500. Inner budgets are the dominant shape (3-12s), so this check alone closes the
// smaller half.
//
// Trade-off: a genuine internal fault reported after the deadline is relabelled a
// timeout; the handler's ERROR log still carries the real error.
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

// clientAborted reports whether a reader-returned error came from the client
// cancelling its request. When true, handlers SHOULD return without writing: the
// client is gone, and obs.HTTPMetrics labels it 499 rather than the 500 a
// writeProblem would produce.
//
// Rule: the request context must be done AND its cause CANCELLATION
// ([context.Canceled], what net/http sets when the peer hangs up).
//
// A done context with [context.DeadlineExceeded] is a SERVER-side budget expiring
// with the client still on the wire (a handler's context.WithTimeout, or the blanket
// middleware.RequestTimeout). Testing only `Err() != nil` would conflate it: the
// handler would return silently and net/http would emit a BODYLESS 200, an
// authoritative-looking empty result (a Blend pool with real supply would render
// "0 reserves / $0 TVL"). Deadlines belong on the 503 problem+json path.
//
// The 499 relabel can't cover it: obs.HTTPMetrics sits OUTSIDE
// middleware.RequestTimeout, so the context it inspects is un-deadlined and the
// recorder's default 200 stands, counting into http_request_success_duration_seconds,
// the latency SLO's success numerator.
//
// Handler order: clientAborted -> handlerTimedOut (503 timeout) -> 500.
// `err` is unused for the decision but kept so call sites stay stable.
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
// A true verdict is also recorded for usage metering
// ([middleware.MarkReadDeadline]): a timed-out read is billable.
func handlerTimedOut(callCtx context.Context, err error) bool {
	timedOut := errors.Is(err, context.DeadlineExceeded) ||
		callCtx.Err() == context.DeadlineExceeded
	if timedOut {
		middleware.MarkReadDeadline(callCtx)
	}
	return timedOut
}

// transientStorageErr reports whether a storage-layer error looks like a transient
// infrastructure hiccup rather than a server bug. Handlers SHOULD map true to a
// retryable 503, since the sla-probe's availability_pct counts 5xx as failure.
//
// It catches:
//   - SQLSTATE 57014 NOT carried by a context cancellation: Postgres
//     `canceling statement due to user request` on server-side statement_timeout,
//     lock_timeout or idle_in_transaction_session_timeout, which trip neither
//     [clientAborted] nor [handlerTimedOut].
//   - `driver: bad connection`: a backend killed between checkout and execution.
//   - EOF / broken pipe between the api binary and postgres or redis.
//
// The SQLSTATE match is INTENTIONALLY string-based to avoid widening the handler
// layer's dependency on pgconn's typed error. `57014` is stable wire format (pgx
// renders `(SQLSTATE 57014)`).
//
// Caller order: clientAborted, handlerTimedOut, transientStorageErr, then 500.
func transientStorageErr(err error) bool {
	if err == nil {
		return false
	}
	// Postgres UNREACHABLE, not merely slow. Every substring
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
