package obs

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

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
// http.Request.Pattern (populated as a side-effect of
// http.ServeMux.Handler). Middleware inspects it post-dispatch.
//
// For unmatched routes (404) the pattern is empty; we label those
// as `"unmatched"` to keep cardinality bounded.
func HTTPMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := routeFromPattern(r.Pattern)
		elapsed := time.Since(start).Seconds()
		HTTPRequestDuration.WithLabelValues(r.Method, route).Observe(elapsed)
		HTTPRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
	})
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
