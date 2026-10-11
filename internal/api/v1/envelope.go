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
	// CoverageFrom is the earliest instant the served tier holds price history for the requested
	// pair: the bottom of the range an empty response can speak for. It sits on the envelope
	// because one of the surfaces that need it returns a bare trade array with no object to hang
	// it on. Absent means UNKNOWN, never "from the beginning of time".
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

// Flags are the advisory quality markers.
//   - Stale: below this surface's documented baseline contract (e.g. /v1/price degraded to
//     last-trade). Not used on /v1/price/tip's last-good fallback.
//   - ReducedRedundancy: cross-region redundancy degraded. Triangulated: computed via a pivot
//     (typically USD), not a directly-traded pair.
//   - DivergenceWarning: cross-reference/anomaly detection saw a meaningful divergence.
//   - Frozen: anomaly detection refused the new bucket; the response carries the held
//     last-known-good value with its own observed_at, and Stale set. On /v1/price,
//     /v1/price/batch and SEP-40 lastprice/x_last_price; tip and observations ignore freeze.
//   - OutsideCoverage: the range ends at or before the pair's `coverage_from`, so the empty answer
//     is a coverage statement. Only on windowed surfaces, only when the floor is KNOWN, never on a
//     range straddling it.
//   - SingleSource: one contributing source; with Frozen, the manipulation signature.
//   - Diverged / Rerouted / PivotUnverified: only on the /v1/price triangulated path. Routes
//     DISAGREED / SUBSTITUTED around a dry leg / a leg priced only from par-valued stablecoin
//     prints, with no own-quote prints to check a de-peg against.
type Flags struct {
	Stale             bool `json:"stale"`
	ReducedRedundancy bool `json:"reduced_redundancy"`
	Triangulated      bool `json:"triangulated"`
	DivergenceWarning bool `json:"divergence_warning"`
	// DivergenceChecked is true only when a live cross-reference check ran (>=
	// `min_sources_for_warning` responding references, the worker's quorum). When false the check
	// is blind, so a `false` warning must not be read as "prices agree"; a `true` warning is then
	// the last verdict carried forward.
	//
	// Set only on surfaces that consult the verdict (/v1/price, its ?window= variant when the
	// window matches the verdict's, /v1/price/tip and its stream), each looking up the exact
	// (base, quote) spelling served, never another alias's market (lookupDivergenceFlag).
	// Elsewhere false means "not consulted", never "checked and clean". The tip stream's lookup
	// has a short budget ([tipStreamDivergenceBudget]); a slow store degrades the flag, never
	// the cadence.
	DivergenceChecked bool `json:"divergence_checked"`
	// OutsideCoverage marks an empty answer whose range ends at or before `coverage_from`. Without
	// it an empty series and a quiet market are the same bytes. It stays false when the floor
	// could not be established.
	OutsideCoverage bool `json:"outside_coverage,omitempty"`
	Frozen          bool `json:"frozen,omitempty"`
	// FrozenChecked is true only when the freeze marker was actually read. When false, `frozen`
	// is NOT meaningful and must not be read as "confirmed not frozen" (same posture as
	// DivergenceChecked).
	FrozenChecked bool `json:"frozen_checked,omitempty"`
	SingleSource  bool `json:"single_source,omitempty"`
	// Diverged marks a triangulated composite whose contributing routes disagreed
	// (cachekeys.VWAPCompositeMeta). Set on the /v1/price triangulated path only.
	Diverged bool `json:"diverged,omitempty"`
	// Rerouted marks a triangulated composite that substituted around a DRY configured chain leg,
	// so the price came via an alternative path. Set on the /v1/price triangulated path only.
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
	// UnverifiedTickerCollision fires on `/v1/assets/{id}` when the asset's code matches a verified
	// currency's ticker but its issuer does not (someone issued their own "USDC"). The
	// `unverified_warning` body points at the verified asset. See
	// docs/architecture/supply-pipeline.md#asset-identity.
	UnverifiedTickerCollision bool `json:"unverified_ticker_collision,omitempty"`
	// FiltersIgnored names the row-narrowing query parameters the response did NOT apply, spelled
	// as sent. A listing that dropped a filter and one that matched on it are otherwise the same
	// 200. `/v1/assets` sets it where the row source cannot narrow (class-scoped catalogue
	// listings and the lean AssetReader fallback).
	FiltersIgnored []string `json:"filters_ignored,omitempty"`
	// Degraded marks a 200 whose body is carried forward or partial (stale-while-revalidate past
	// TTL, last-good after a failed refresh, a held frozen price, a dropped best-effort section).
	// Never on the wire: writeEnvelopeStatus turns it into `Cache-Control: no-store` so a shared
	// cache cannot keep serving it after recovery. Set explicitly at each such exit, never derived
	// from Stale, which also covers fresh reads of a lagging source a cache may hold.
	Degraded bool `json:"-"`
}

// Pagination is present on list-returning endpoints only.
type Pagination struct {
	Next string `json:"next,omitempty"`
}

// Problem is the RFC 9457 error payload. Custom fields are snake_case; `Instance` is typically
// the request URL. RequestID echoes X-Request-ID so a body alone finds the server-side trace.
// CoverageFrom / OutsideCoverage repeat the 2xx envelope members on /v1/price/at, whose "no closed
// bucket" 404 has the same dead-market-or-before-history ambiguity.
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

// writeProblem writes an RFC 9457 error response; handlers call it instead of http.Error to keep
// the wire contract consistent. typeURL is the stable error-type URL
// (https://api.stellarindex.io/errors/<name>), title a short headline, detail an optional
// per-request message.
//
// A 500 whose request deadline has already expired becomes the retryable timeout problem (see
// requestDeadlineExpired). That covers the blanket middleware deadline only; a path holding the
// error from a handler's own budget must call writeProblemErr.
func writeProblem(w http.ResponseWriter, r *http.Request, typeURL, title string, status int, detail string) {
	writeProblemCoverage(w, r, typeURL, title, status, detail, nil, false, nil, "")
}

// writeProblemCoverage is [writeProblem] carrying the coverage-floor extension members, used by
// /v1/price/at, whose "nothing there" answer is a 404 rather than an empty array. It is the shared
// body, so every rewrite rule writeProblem applies (deadline to 503, no-store, 401 challenge)
// applies here identically.
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
	// Errors override the per-route cache-control directive, or a CDN would cache a transient
	// 400/404/500 and replay it to other anonymous clients on the same cache key.
	w.Header().Set("Cache-Control", "no-store")
	// RFC 7235 section 3.1 requires a WWW-Authenticate challenge on every 401. The magic-link
	// cookie path has no standard challenge token, so only Bearer is advertised.
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="stellarindex.io"`)
	}
	// Keyed on the type, not the status: writeProblemErr rewrites before calling in, so only the
	// type still identifies the condition. Both upgrade legs thus carry the hint.
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
	// No budget figure in the detail: the bound is the deployment's api.request_timeout, which
	// this package cannot quote truthfully.
	requestTimeoutDetail = "the request exceeded this server's request budget before " +
		"the handler could answer; retry shortly."
	// Without Retry-After clients guess "immediately", a retry storm against a server that just ran
	// out of budget. 5s is the in-process busy value used elsewhere in the API; 30s is reserved
	// for a dependency outage (writeCacheUnavailableProblem).
	retryAfterRequestTimeout = "5"
)

// requestDeadlineExpired reports whether the blanket middleware.RequestTimeout deadline on
// r.Context() has already fired, so writeProblem can upgrade a 500 to a retryable 503 in one
// place; a deadline is capacity, not a fault (the sla-probe books a 500 as a failure).
//
// It cannot see a handler's own shorter context.WithTimeout budget expiring while r.Context() is
// alive, the dominant shape; that is why writeProblemErr exists. A genuine fault reported after
// the deadline is relabelled a timeout; the ERROR log keeps the real error.
func requestDeadlineExpired(r *http.Request) bool {
	return errors.Is(r.Context().Err(), context.DeadlineExceeded)
}

// writeProblemErr is writeProblem for a call site holding the failing error. It rewrites a fault
// status to the retryable request-timeout 503 when the error is a deadline, the case
// requestDeadlineExpired cannot see because a handler's own context.WithTimeout budget expires
// while r.Context() still has budget left.
//
// Both 500 and 502 are rewritten: a deadline is a bound this process imposed. A 4xx stays the
// client's answer; an already-retryable 503 keeps its own type. Keyed on errors.Is, so a driver
// that re-phrases the cancellation (SQLSTATE 57014) is missed; sites with a per-call ctx should
// branch on handlerTimedOut and use this only as the fallback.
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

// clientAborted reports whether a reader-returned error came from the client cancelling its
// request. When true, handlers SHOULD return without writing: the client is gone, and
// obs.HTTPMetrics labels it 499 rather than the 500 a writeProblem would produce.
//
// The cause must be [context.Canceled] (the peer hung up). [context.DeadlineExceeded] is a
// server-side budget expiring with the client still on the wire; returning silently there would
// emit a bodyless 200, an authoritative-looking empty result, and obs.HTTPMetrics (outside
// middleware.RequestTimeout) would count it into the latency SLO's success numerator. Deadlines
// belong on the 503 problem+json path.
//
// Handler order: clientAborted, handlerTimedOut (503), then 500. `err` is unused but kept so call
// sites stay stable.
func clientAborted(r *http.Request, _ error) bool {
	return errors.Is(r.Context().Err(), context.Canceled)
}

// handlerTimedOut reports whether a handler-scoped context (from context.WithTimeout capping one
// storage call) hit its deadline. Use it on the per-call context, NOT r.Context().
//
// The Postgres driver cancels via the v3 cancel-request protocol and returns `canceling
// statement due to user request` (SQLSTATE 57014), which does NOT unwrap to
// [context.DeadlineExceeded], so the per-call context's Err() is the reliable signal. A true
// verdict is recorded for usage metering ([middleware.MarkReadDeadline]): a timed-out read is
// billable.
func handlerTimedOut(callCtx context.Context, err error) bool {
	timedOut := errors.Is(err, context.DeadlineExceeded) ||
		callCtx.Err() == context.DeadlineExceeded
	if timedOut {
		middleware.MarkReadDeadline(callCtx)
	}
	return timedOut
}

// transientStorageErr reports whether a storage-layer error looks like a transient infrastructure
// hiccup rather than a server bug. Handlers SHOULD map true to a retryable 503, since the
// sla-probe's availability_pct counts 5xx as failure.
//
// It catches SQLSTATE 57014 not carried by a context cancellation (server-side statement_timeout,
// lock_timeout, idle_in_transaction_session_timeout), `driver: bad connection`, and EOF / broken
// pipe to postgres or redis. The SQLSTATE match is string-based so the handler layer does not
// depend on pgconn's typed error; `57014` is stable wire format.
//
// Caller order: clientAborted, handlerTimedOut, transientStorageErr, then 500.
func transientStorageErr(err error) bool {
	if err == nil {
		return false
	}
	// Unreachable, not merely slow: the substring arms below describe a connection that existed and
	// misbehaved, and none matches pgx's failure to establish one, which a restarting or
	// failed-over Postgres produces. Without this arm such an outage is a 500 that looks like a bug.
	if unreachableStorageErr(err) {
		return true
	}
	s := err.Error()
	// Postgres-side cancellation; client-side cancellation is clientAborted's.
	if strings.Contains(s, "57014") || strings.Contains(s, "canceling statement") {
		return true
	}
	if strings.Contains(s, "driver: bad connection") || strings.Contains(s, "bad connection") {
		return true
	}
	if strings.Contains(s, "broken pipe") || strings.Contains(s, "connection reset") ||
		strings.Contains(s, "unexpected EOF") || strings.Contains(s, "EOF") {
		return true
	}
	return false
}

// unreachableStorageErr reports whether err is a failure to reach the storage dependency (dial
// refused, socket died, host stopped resolving). Structural checks come first because they survive
// rewording (pgx v5 wraps its dial failure in *pgconn.ConnectError, unwrapping to *net.OpError);
// the substrings cover wrappers that render the cause into a string.
//
// Keep in step with the ClickHouse-side lakeUnreachable (explorer/reader.go), which cannot import
// this package (v1.Server embeds its Handler).
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
