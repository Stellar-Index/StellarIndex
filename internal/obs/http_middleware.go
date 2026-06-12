package obs

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// routeCapture is a single-field pointer holder we plant in the
// request context so HTTPMetrics can read the matched route after
// the inner mux has dispatched. See [CaptureRoute] for the writer
// side and [HTTPMetrics] for why we need this indirection rather
// than reading r.Pattern directly.
type routeCapture struct{ route string }

type routeCaptureKey struct{}

// HTTPMetrics returns middleware that emits `http_requests_total`
// + `http_request_duration_seconds` for every served request.
//
// Label discipline:
//   - `method`: the HTTP verb (uppercase).
//   - `route`: the registered route pattern path (e.g. "/v1/assets/{asset_id}"),
//     NOT the raw URL — using the raw URL would blow up cardinality
//     on endpoints with ID path params. The method prefix is stripped
//     from Go 1.22+ patterns so it doesn't duplicate `method`.
//   - `status`: HTTP status code as a string; dashboards regex-filter
//     (status=~"5..") for bucketing.
//
// # Route pattern discovery
//
// Go 1.22+ ServeMux exposes the matched pattern via
// http.Request.Pattern, but only on the request struct the mux was
// dispatched with — and any middleware between HTTPMetrics and the
// mux that calls `r = r.WithContext(...)` (Logger does, to attach
// request_id / remote_ip) creates a fresh struct, leaving
// HTTPMetrics holding a Request whose Pattern stays "".
//
// To survive the WithContext shadow-copy chain we plant a
// *routeCapture pointer in the request context. The innermost
// [CaptureRoute] middleware writes r.Pattern into it after
// dispatch; HTTPMetrics reads from the same pointer. The pointer
// itself is in the context, and contexts pass through WithContext
// chains unchanged, so all middlewares see the same routeCapture.
//
// For unmatched routes (404) the pattern is empty; we label those
// as `"unmatched"` to keep cardinality bounded.
func HTTPMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		rc := &routeCapture{}
		ctx := context.WithValue(r.Context(), routeCaptureKey{}, rc)
		// Keep a reference to the ctx-wrapped request so we can
		// read the mux-set Pattern off it as a fallback when no
		// CaptureRoute is wired innermost. Reading from the
		// original `r` here would always be empty — our own
		// WithContext above shadowed it.
		r2 := r.WithContext(ctx)
		next.ServeHTTP(rec, r2)

		route := rc.route
		if route == "" {
			route = routeFromPattern(r2.Pattern)
		}
		method := normalizeMethod(r.Method)
		elapsed := time.Since(start).Seconds()

		// Client-abort detection. If the request's context was
		// cancelled before the handler finished writing, record the
		// NGINX-style 499 "client closed request" sentinel instead
		// of whatever status the recorder saw — otherwise an early
		// disconnect looks like a successful 200 on the dashboard.
		status := rec.status
		if err := r.Context().Err(); err != nil && !rec.wrote {
			status = 499
		}

		// Skip metrics emission for synthetic monitoring traffic.
		// The smoke timer (configs/healthchecks/r1-smoke.sh) and
		// other operator-side probes set
		// `User-Agent: stellarindex-smoke/<n>` so we can identify
		// them. Letting their requests into the histogram pollutes
		// the SLO recording rule — at every smoke fire we cold-hit
		// /v1/oracle/latest etc., adding 13 slow-request samples
		// every 5 minutes that customers never experience. The
		// alerts then fire on a synthetic-monitoring artifact
		// rather than real customer-facing latency.
		//
		// Smoke traffic still exits the process and the response
		// is real — we just don't surface it in the customer-facing
		// observability stream. Failures are caught by the smoke
		// script's exit code + Healthchecks.io ping (see
		// configs/healthchecks/smoke.sh).
		if isSyntheticUA(r.UserAgent()) {
			return
		}

		HTTPRequestsTotal.WithLabelValues(method, route, strconv.Itoa(status)).Inc()

		// SSE / long-lived streaming endpoints: the handler returns
		// only when the client disconnects, so `elapsed` is the
		// connection LIFETIME (minutes-to-hours), not request
		// latency. Feeding that into the latency histogram pins p99
		// at the +Inf bucket (the histogram tops out at 10s) and
		// burns the latency SLO — one open status-page tab on
		// /v1/ledger/stream is enough to do it. Count the request
		// (above) but skip the duration observation.
		if isStreamingRoute(route) {
			return
		}
		HTTPRequestDuration.WithLabelValues(method, route).Observe(elapsed)
		// F-0105: only count non-5xx in the success histogram. The
		// latency SLO numerator filters on this histogram while the
		// denominator counts everything via HTTPRequestDuration, so a
		// fast 500 lands in the denominator but not the numerator —
		// SLO ratio drops, budget burns, alert fires. Pre-this-PR
		// both numerator + denominator pulled from the same histogram
		// and a fast 500 reported as "good." 499 (client-aborted)
		// also stays out: it isn't a service-side failure but it
		// isn't customer success either; safer to exclude than to
		// dilute the numerator.
		if status < 500 && status != 499 {
			HTTPRequestSuccessDuration.WithLabelValues(method, route).Observe(elapsed)
		}
	})
}

// isStreamingRoute reports whether a route pattern is an SSE /
// long-lived streaming endpoint, identified by the conventional
// `/stream` suffix (`/v1/price/stream`, `/v1/price/tip/stream`,
// `/v1/observations/stream`, `/v1/ledger/stream`). Such handlers
// must be kept out of the request-latency histogram — see the
// caller. Suffix-matching is deliberate so any future `/stream`
// route is excluded automatically.
func isStreamingRoute(route string) bool {
	return strings.HasSuffix(route, "/stream")
}

// isSyntheticUA reports whether the User-Agent identifies internal
// synthetic / maintenance traffic that must not pollute the
// customer-facing SLO. Matches `stellarindex-smoke/...` (the
// r1-smoke.sh wrapper), `stellarindex-probe/...` (operator probes),
// and `stellarindex-prewarm/...` (the API's own self-prewarm
// goroutine, which HTTP-GETs its endpoints to warm caches — its
// requests are deliberately cold and would otherwise dominate the
// latency histogram). The match is prefix-only so version suffixes
// don't affect the decision.
func isSyntheticUA(ua string) bool {
	if ua == "" {
		return false
	}
	for _, prefix := range syntheticUAPrefixes {
		if strings.HasPrefix(ua, prefix) {
			return true
		}
	}
	return false
}

var syntheticUAPrefixes = []string{
	"stellarindex-smoke/",
	"stellarindex-probe/",
	"stellarindex-prewarm/",
}

// CaptureRoute writes the mux-matched route pattern into the
// *routeCapture installed by [HTTPMetrics]. Wire this as the
// INNERMOST middleware in the stack — directly above the mux —
// so r.Pattern is populated before this middleware reads it.
//
// No-op when the request context doesn't carry a routeCapture —
// the route still ends up in r.Pattern; HTTPMetrics's fallback
// path picks it up.
func CaptureRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		rc, ok := r.Context().Value(routeCaptureKey{}).(*routeCapture)
		if !ok {
			return
		}
		rc.route = routeFromPattern(r.Pattern)
	})
}

// normalizeMethod canonicalises the HTTP method label. HTTP's spec
// treats method names as case-sensitive, but in practice standard
// methods are always uppercase — a client sending "get" instead of
// "GET" would otherwise double our method-label cardinality.
// Unknown methods pass through as-is so legit custom verbs
// (WebDAV PROPFIND, etc.) still work.
func normalizeMethod(m string) string {
	switch strings.ToUpper(m) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "CONNECT", "TRACE":
		return strings.ToUpper(m)
	}
	return m
}

// routeFromPattern extracts just the path from a Go 1.22+ ServeMux
// pattern. "METHOD /path" → "/path"; "/path" → "/path"; "" →
// "unmatched".
func routeFromPattern(p string) string {
	if p == "" {
		return "unmatched"
	}
	if i := strings.IndexByte(p, ' '); i >= 0 {
		return p[i+1:]
	}
	return p
}

// statusRecorder wraps http.ResponseWriter + captures status. Tiny
// duplicate of the one in middleware/logger.go — kept here so obs
// doesn't depend on the middleware package (which imports obs in
// the production wiring).
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wrote {
		return
	}
	r.wrote = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush preserves http.Flusher for SSE endpoints.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying ResponseWriter so
// http.NewResponseController can reach SetWriteDeadline / Hijack
// on it. Required for SSE handlers that need to clear the global
// 30s WriteTimeout so long-running streams don't get cut.
// F-1228 (codex audit-2026-05-12).
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
