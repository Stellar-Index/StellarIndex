package obs

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// routeCapture carries the matched route out of the mux; see [HTTPMetrics].
type routeCapture struct{ route string }

type routeCaptureKey struct{}

// HTTPMetrics emits http_requests_total and http_request_duration_seconds labelled by
// method, route pattern (never the raw URL, for cardinality; "unmatched" on 404) and status.
// r.Pattern is lost once any middleware calls WithContext, so a *routeCapture planted in
// the context lets the innermost [CaptureRoute] hand the pattern back out.
func HTTPMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		rc := &routeCapture{}
		ctx := context.WithValue(r.Context(), routeCaptureKey{}, rc)
		// Keep the ctx-wrapped request: its Pattern is the fallback when no CaptureRoute is wired.
		r2 := r.WithContext(ctx)
		next.ServeHTTP(rec, r2)

		route := rc.route
		if route == "" {
			route = routeFromPattern(r2.Pattern)
		}
		method := normalizeMethod(r.Method)
		elapsed := time.Since(start).Seconds()

		// A client abort before any write is 499, not the recorder's default 200.
		status := rec.status
		if err := r.Context().Err(); err != nil && !rec.wrote {
			status = 499
		}

		// Synthetic probes (smoke, SLA probe, prewarm) stay out of the SLO series: their cold hits
		// would fire latency alerts customers never see. Their failures surface via Healthchecks.
		if IsSyntheticRequest(r) {
			return
		}

		HTTPRequestsTotal.WithLabelValues(method, route, strconv.Itoa(status)).Inc()

		// A stream's elapsed time is connection lifetime, which would pin p99 at +Inf; count it
		// but skip the duration.
		if isStreamingRoute(route) {
			return
		}
		HTTPRequestDuration.WithLabelValues(method, route).Observe(elapsed)
		// The success histogram is the latency SLO numerator, so 5xx (a fast 500 is not good)
		// and 499 (not a customer success) stay out of it.
		if status < 500 && status != 499 {
			HTTPRequestSuccessDuration.WithLabelValues(method, route).Observe(elapsed)
		}
	})
}

// isStreamingRoute matches the `/stream` suffix so a future SSE route is excluded automatically.
func isStreamingRoute(route string) bool {
	return strings.HasSuffix(route, "/stream")
}

// IsSyntheticUA is a NAME check on the client-controlled User-Agent, never a trust check;
// gate on [IsSyntheticRequest].
func IsSyntheticUA(ua string) bool {
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

// IsSyntheticRequest is the trust boundary for skipping SLO metrics and demoting logs
// (middleware.Logger must agree). A spoofable UA counts only on loopback with no
// X-Forwarded-For: haproxy appends that header to every proxied request, and the three
// first-party callers hit the loopback listener directly.
func IsSyntheticRequest(r *http.Request) bool {
	if !IsSyntheticUA(r.UserAgent()) {
		return false
	}
	if r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	return isLoopbackRemoteAddr(r.RemoteAddr)
}

// isLoopbackRemoteAddr reports whether a request's RemoteAddr
// (host[:port] or bare host) resolves to a loopback address.
func isLoopbackRemoteAddr(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CaptureRoute must be the INNERMOST middleware so r.Pattern is set. Deferred so a panic
// recovered further out still labels its 500 with the route.
func CaptureRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, ok := r.Context().Value(routeCaptureKey{}).(*routeCapture)
		if ok {
			defer func() { rc.route = routeFromPattern(r.Pattern) }()
		}
		next.ServeHTTP(w, r)
	})
}

// RouteFromContext returns the route [CaptureRoute] captured, or "". Middleware between
// HTTPMetrics and CaptureRoute needs it because its own request copy never sees Pattern.
func RouteFromContext(ctx context.Context) string {
	rc, ok := ctx.Value(routeCaptureKey{}).(*routeCapture)
	if !ok {
		return ""
	}
	return rc.route
}

// normalizeMethod maps unknown verbs to "other": net/http accepts any token as a method,
// so passing them through lets an unauthenticated client mint unbounded label children
// (cardinality DoS). Routing still sees the real r.Method.
func normalizeMethod(m string) string {
	switch strings.ToUpper(m) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "CONNECT", "TRACE":
		return strings.ToUpper(m)
	}
	return "other"
}

// routeFromPattern strips the method from a ServeMux pattern; "" becomes "unmatched".
func routeFromPattern(p string) string {
	if p == "" {
		return "unmatched"
	}
	if i := strings.IndexByte(p, ' '); i >= 0 {
		return p[i+1:]
	}
	return p
}

// statusRecorder duplicates middleware's recorder because middleware imports obs.
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

// Unwrap lets http.NewResponseController clear the 30s WriteTimeout for SSE streams.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
