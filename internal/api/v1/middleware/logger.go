package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// Logger emits one structured log entry per request:
//   - 5xx → ERROR
//   - 4xx (except 429) → WARN
//   - 429 → skipped (see below)
//   - everything else → INFO
//
// Fields (minimum):
//   - method, path, status, bytes, latency_ms
//   - request_id (from RequestID middleware)
//   - remote_ip (X-Forwarded-For first hop if present, else
//     r.RemoteAddr stripped of the port)
//   - user_agent
//
// 429 special case: a single misconfigured client (or a load
// generator without an API key) can produce thousands of 429s per
// second on a public origin. r1 evidence on 2026-05-04 — a 60-second
// 4-worker probe run produced 343 k suppressed `systemd-journald`
// entries before journald's own rate limiter kicked in, dropping
// other-service messages that operators would actually want.
// Visibility is preserved by the
// `stellarindex_http_requests_total{status="429"}` counter (see
// `internal/obs/http_middleware.go`); the per-line log adds journal
// pressure without diagnostic value the metric doesn't already
// carry.
//
// Does NOT log query parameters or request bodies — they may
// carry API keys or PII. Add named fields in specific handlers
// when needed.
// SlowRequestThreshold is the latency at or above which a request also
// logs its query shape and a `slow=true` marker.
//
// 500 ms is the published p99 SLA target, so "slow" here means "outside
// the number we promise" rather than an arbitrary line. On r1 that
// selected ~0.2% of requests (48 of 21,104 in a 30-minute sample), which
// is the property that matters: the field has to be rare enough not to
// re-create the journal pressure this file's 429 note is about.
const SlowRequestThreshold = 500 * time.Millisecond

func Logger(logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			remote := resolveRemoteIP(r)
			ctx := withString(r.Context(), ctxKeyRemoteIP, remote)
			r = r.WithContext(ctx)

			// Wrap the writer so we capture status + bytes without
			// breaking http.ResponseController.
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			if rec.status == http.StatusTooManyRequests {
				return
			}

			latency := time.Since(start)
			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"latency_ms", float64(latency.Microseconds()) / 1000.0,
				"request_id", RequestIDFrom(r),
				"remote_ip", remote,
				"user_agent", r.UserAgent(),
			}

			// A slow request additionally carries its query SHAPE. On
			// this API the query string is what decides the query plan —
			// `/v1/assets` is one route and many plans, selected by limit,
			// order_by and cursor — so `path` alone cannot tell an
			// operator which request was slow. Measured on r1 2026-09-01:
			// /v1/assets?limit=100 served in 82 ms while the same limit
			// with order_by=volume_24h_usd_desc took 1523 ms. Identical
			// log lines, 18x apart.
			//
			// Values are allow-listed, never raw — see QueryShape. The
			// field is attached only above the threshold, which keeps the
			// journal-pressure objection in this file's own doc satisfied:
			// on r1 this is ~0.2% of requests.
			if latency >= SlowRequestThreshold {
				if shape := QueryShapeOf(r); shape != "" {
					attrs = append(attrs, "query_shape", shape)
				}
				attrs = append(attrs, "slow", true)
			}

			switch {
			case rec.status >= 500:
				logger.Error("http request", attrs...)
			case rec.status >= 400:
				logger.Warn("http request", attrs...)
			default:
				logger.Info("http request", attrs...)
			}
		})
	}
}

// statusRecorder wraps an http.ResponseWriter + captures status +
// byte count. The bare minimum — no special interface passes-through
// (h2, flusher, hijacker). Re-evaluate when we add SSE (which
// needs Flusher).
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
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
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap exposes the underlying ResponseWriter so
// http.NewResponseController can reach SetWriteDeadline / Flush /
// Hijack on it. Without this, SSE handlers (which need
// SetWriteDeadline(zero-Time) to dodge the global 30s WriteTimeout)
// would see http.ErrNotSupported on every middleware-wrapped
// connection in production. F-1228 (codex audit-2026-05-12).
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Flush preserves http.Flusher for SSE endpoints — without this,
// wrapping breaks chunked streaming.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
