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

// Envelope is the shape of every 2xx JSON response (docs/reference/api-design.md section 4).
type Envelope struct {
	Data any      `json:"data"`
	AsOf WireTime `json:"as_of"`
	// CoverageFrom is the earliest instant the served tier holds price history for the pair. It is
	// on the envelope because one surface returns a bare trade array. Absent means UNKNOWN, never
	// "from the beginning of time".
	CoverageFrom *WireTime `json:"coverage_from,omitempty"`
	// Withheld names ids /v1/price/batch omitted because a serving gate declined to publish their
	// price, as distinct from ids with no data.
	Withheld []string `json:"withheld,omitempty"`
	// Thin names ids /v1/price/batch served under `include_thin=true` from a sub-floor market.
	Thin []string `json:"thin,omitempty"`
	// Stale and Triangulated name the /v1/price/batch rows behind the OR-ed flags of the same name,
	// so a multi-id batch keeps each row's provenance.
	Stale        []string    `json:"stale,omitempty"`
	Triangulated []string    `json:"triangulated,omitempty"`
	Sources      []string    `json:"sources,omitempty"`
	Flags        Flags       `json:"flags"`
	Pagination   *Pagination `json:"pagination,omitempty"`
}

// Flags are the advisory quality markers (Stale, Triangulated, Frozen, OutsideCoverage, SingleSource, Diverged, Rerouted, PivotUnverified); OutsideCoverage is set only when the coverage floor is KNOWN and the range ends at or before it.
type Flags struct {
	Stale             bool `json:"stale"`
	ReducedRedundancy bool `json:"reduced_redundancy"`
	Triangulated      bool `json:"triangulated"`
	DivergenceWarning bool `json:"divergence_warning"`
	// DivergenceChecked is true only when a live cross-reference check ran (>=
	// `min_sources_for_warning` responding references). When false the check is blind: a `false`
	// warning is not "prices agree" and a `true` one is the last verdict carried forward.
	//
	// Set only where the verdict is consulted, for the exact (base, quote) spelling served, never
	// another alias's market (lookupDivergenceFlag). Elsewhere false means "not consulted". The tip
	// stream's lookup has a short budget ([tipStreamDivergenceBudget]); a slow store degrades the
	// flag, never the cadence.
	DivergenceChecked bool `json:"divergence_checked"`
	// OutsideCoverage marks an empty answer whose range ends at or before `coverage_from`, which
	// otherwise looks identical to a quiet market. False when the floor is unknown.
	OutsideCoverage bool `json:"outside_coverage,omitempty"`
	Frozen          bool `json:"frozen,omitempty"`
	// FrozenChecked is true only when the freeze marker was actually read; when false, `frozen` is
	// NOT "confirmed not frozen" (same posture as DivergenceChecked).
	FrozenChecked bool `json:"frozen_checked,omitempty"`
	SingleSource  bool `json:"single_source,omitempty"`
	// Diverged marks a triangulated composite whose contributing routes disagreed.
	Diverged bool `json:"diverged,omitempty"`
	// Rerouted marks a triangulated composite that substituted around a DRY configured chain leg.
	Rerouted bool `json:"rerouted,omitempty"`
	// PivotUnverified: a composite leg was all stablecoin prints at par, so a de-peg in it went unchecked.
	PivotUnverified bool `json:"pivot_unverified,omitempty"`
	// ThinMarket: served from a sub-floor market, under `include_thin=true` or by /v1/vwap and
	// /v1/twap, which serve such a market by default.
	ThinMarket bool `json:"thin_market,omitempty"`
	// ProxyDeviation: a TRIANGULATED fiat:USD price assumes a declared USD peg is $1, and a declared
	// peg's observed dollar price is more than 2% from $1.
	ProxyDeviation bool `json:"proxy_deviation,omitempty"`
	// UnverifiedTickerCollision fires on `/v1/assets/{id}` when the asset's code matches a verified
	// currency's ticker but its issuer does not (someone issued their own "USDC"); the
	// `unverified_warning` body points at the verified asset.
	UnverifiedTickerCollision bool `json:"unverified_ticker_collision,omitempty"`
	// DetectorsPartial: /v1/mev when the aggregator's MEV worker has not completed a run within the
	// heartbeat TTL, its last run did not resolve every lake tx-order hash (sandwich and
	// oracle_sandwich ran on a partial map or not at all), or the heartbeat could not be read. A
	// quiet feed under this flag is a detection gap, not a quiet network.
	DetectorsPartial bool `json:"detectors_partial,omitempty"`
	// FiltersIgnored names the row-narrowing query parameters the response did NOT apply, spelled
	// as sent; a listing that dropped a filter is otherwise the same 200 as one that matched.
	FiltersIgnored []string `json:"filters_ignored,omitempty"`
	// Degraded marks a 200 whose body is carried forward or partial (stale-while-revalidate past
	// TTL, last-good after a failed refresh, a held frozen price, a dropped best-effort section).
	// Never on the wire: writeEnvelopeStatus turns it into `Cache-Control: no-store` so a shared
	// cache cannot keep serving it after recovery. Set explicitly, never derived from Stale, which
	// also covers fresh reads of a lagging source.
	Degraded bool `json:"-"`
}

// Pagination is present on list-returning endpoints only.
type Pagination struct {
	Next string `json:"next,omitempty"`
}

// Problem is the RFC 9457 error payload; RequestID echoes X-Request-ID, and CoverageFrom/OutsideCoverage repeat the envelope members on /v1/price/at.
type Problem struct {
	Type            string    `json:"type"`
	Title           string    `json:"title"`
	Status          int       `json:"status"`
	Detail          string    `json:"detail,omitempty"`
	Instance        string    `json:"instance,omitempty"`
	RequestID       string    `json:"request_id,omitempty"`
	CoverageFrom    *WireTime `json:"coverage_from,omitempty"`
	OutsideCoverage bool      `json:"outside_coverage,omitempty"`
	// Substance is the measurement behind a thin-market price-withheld verdict.
	Substance *SubstanceEvidence `json:"substance,omitempty"`
	// Reason is the machine-readable [PriceWithheldReason], the same enum SSE and asset rows carry.
	Reason PriceWithheldReason `json:"reason,omitempty"`
}

// writeJSON writes the Envelope + 200.
func writeJSON(w http.ResponseWriter, data any, flags Flags, sources ...string) {
	writeEnvelope(w, Envelope{
		Data:    data,
		AsOf:    WireTime(time.Now().UTC()),
		Sources: sources,
		Flags:   flags,
	})
}

// dataVintage folds the fill times of the cached reads behind a response into its as_of: the oldest fill when all were cached
// (unchanged payload replays byte-identical, ETag 304s), else now.
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

// writeJSONCoverage is [writeJSON] plus the coverage-floor annotation; a nil coverageFrom (the
// probe reached no answer) leaves the field off the wire.
func writeJSONCoverage(w http.ResponseWriter, data any, flags Flags, coverageFrom *time.Time) {
	writeEnvelope(w, Envelope{
		Data:         data,
		AsOf:         WireTime(time.Now().UTC()),
		CoverageFrom: wireTimePtr(coverageFrom),
		Flags:        flags,
	})
}

// writeEnvelope writes a pre-constructed Envelope with a 200.
func writeEnvelope(w http.ResponseWriter, env Envelope) {
	writeEnvelopeStatus(w, http.StatusOK, env)
}

// writeEnvelopeStatus writes a pre-constructed Envelope with an explicit 2xx status.
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

// writeProblem writes an RFC 9457 error; typeURL is the stable error-type URL (https://api.stellarindex.io/errors/<name>). A
// 500 whose request deadline already expired becomes the retryable timeout problem; this covers only the blanket middleware
// deadline, so a handler's own budget must use writeProblemErr.
func writeProblem(w http.ResponseWriter, r *http.Request, typeURL, title string, status int, detail string) {
	writeProblemCoverage(w, r, typeURL, title, status, detail, nil, false, nil, "")
}

// writeProblemCoverage is [writeProblem] carrying the coverage-floor extension members. It is the
// shared body, so every rewrite rule (deadline to 503, no-store, 401 challenge) applies identically.
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
	// Errors override the per-route cache-control, or a CDN would replay a transient 400/404/500.
	w.Header().Set("Cache-Control", "no-store")
	// RFC 7235 requires a challenge on every 401; the magic-link cookie path has none, so only Bearer.
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="stellarindex.io"`)
	}
	// Keyed on the type, not the status: writeProblemErr rewrites before calling in, so only the
	// type still identifies the condition.
	if typeURL == requestTimeoutType {
		middleware.MarkReadDeadline(r.Context())
		w.Header().Set("Retry-After", retryAfterRequestTimeout)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// The canonical problem writeProblem substitutes for a 500 raised after the request deadline expired.
const (
	requestTimeoutType  = "https://api.stellarindex.io/errors/request-timeout"
	requestTimeoutTitle = "Request timed out"
	// No budget figure in the detail: the bound is the deployment's api.request_timeout, which
	// this package cannot quote truthfully.
	requestTimeoutDetail = "the request exceeded this server's request budget before " +
		"the handler could answer; retry shortly."
	// Without Retry-After clients retry immediately, a storm against a server that just ran out of
	// budget. 30s is reserved for a dependency outage (writeCacheUnavailableProblem).
	retryAfterRequestTimeout = "5"
)

// requestDeadlineExpired reports whether the blanket middleware.RequestTimeout fired, so writeProblem can upgrade a 500 to a
// retryable 503 (capacity, not fault). It cannot see a handler's own shorter budget expiring (hence writeProblemErr); the ERROR
// log keeps the real error.
func requestDeadlineExpired(r *http.Request) bool {
	return errors.Is(r.Context().Err(), context.DeadlineExceeded)
}

// writeProblemErr is writeProblem for a site holding the error: a deadline error rewrites a 500/502 to the retryable timeout
// 503; a 4xx stays, a 503 keeps its type. errors.Is misses a driver-rephrased cancellation (SQLSTATE 57014), so sites with a
// per-call ctx should branch on handlerTimedOut first.
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

// clientAborted reports whether the client cancelled (handlers SHOULD return without writing; obs.HTTPMetrics labels it 499).
// Only [context.Canceled] counts: a DeadlineExceeded is a server budget expiring, and returning silently would emit a bodyless
// 200 counted into the latency SLO. Order: clientAborted, handlerTimedOut (503), then 500.
func clientAborted(r *http.Request, _ error) bool {
	return errors.Is(r.Context().Err(), context.Canceled)
}

// handlerTimedOut reports whether a handler-scoped context hit its deadline; use it on the per-call context, NOT r.Context().
// Postgres 57014 does not unwrap to [context.DeadlineExceeded], so the context's Err() is the signal. A true verdict is
// recorded for metering ([middleware.MarkReadDeadline]): a timed-out read is billable.
func handlerTimedOut(callCtx context.Context, err error) bool {
	timedOut := errors.Is(err, context.DeadlineExceeded) ||
		callCtx.Err() == context.DeadlineExceeded
	if timedOut {
		middleware.MarkReadDeadline(callCtx)
	}
	return timedOut
}

// transientStorageErr reports whether a storage error looks like transient infrastructure (handlers SHOULD map true to
// retryable 503): SQLSTATE 57014 without cancellation, bad connections, EOF/broken pipe. String-matched so this layer avoids
// pgconn types. Order: clientAborted, handlerTimedOut, transientStorageErr, then 500.
func transientStorageErr(err error) bool {
	if err == nil {
		return false
	}
	// The substring arms below describe a connection that existed and misbehaved; none matches
	// pgx's failure to establish one (restarting or failed-over Postgres), which would be a 500.
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

// unreachableStorageErr reports whether err is a failure to reach storage. Structural checks first (they survive rewording);
// substrings cover wrappers. Keep in step with ClickHouse-side lakeUnreachable (explorer/reader.go), which cannot import this
// package.
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
