package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// SlowRequestThreshold is the latency at or above which a request also
// logs its query shape and a `slow=true` marker.
//
// 500 ms is the published p99 SLA target, so "slow" here means "outside
// the number we promise" rather than an arbitrary line. On r1 that
// selected ~0.2% of requests (48 of 21,104 in a 30-minute sample), which
// is the property that matters: the field has to be rare enough not to
// re-create the journal pressure this file's 429 note is about.
const SlowRequestThreshold = 500 * time.Millisecond

// maxLoggedPathLen and maxLoggedUserAgentLen bound how much of these
// two caller-controlled fields reach the journal per line. Neither is
// allow-listed the way QueryShape's parameters are, and nothing else
// bounds them application-side — a path or User-Agent is otherwise
// limited only by Go's net/http default header/request-line size
// (~1 MB), which is a transport limit, not a log-safety one. Paths
// here can legitimately carry a 56-char Stellar contract/account id
// plus nested resource segments, so the path bound is generous; the
// User-Agent bound matches real browser/SDK strings with room to
// spare.
const (
	maxLoggedPathLen      = 512
	maxLoggedUserAgentLen = 256
)

// Logger emits one structured log entry per request:
//   - 5xx -> ERROR
//   - 4xx (except 429) -> WARN
//   - 429 -> skipped (see below)
//   - a SUCCESSFUL request from first-party synthetic traffic -> DEBUG
//   - everything else -> INFO
//
// Fields (minimum): method, path, status, bytes, latency_ms, request_id (from
// RequestID middleware), user_agent, and remote_ip ([RemoteIP]: the rightmost
// X-Forwarded-For hop outside the trusted-proxy CIDRs when the peer is a trusted
// proxy, else r.RemoteAddr stripped of the port; never the first hop, which the
// caller writes).
//
// 429 is skipped: one misconfigured client can produce thousands per second on a
// public origin and flood journald, dropping other services' messages. Visibility
// is kept by the `stellarindex_http_requests_total{status="429"}` counter
// (`internal/obs/http_middleware.go`).
//
// Synthetic traffic goes to DEBUG, the same judgement the SLO uses
// ([obs.IsSyntheticRequest]); the SLA probe would otherwise dominate the journal.
// Only SUCCESSFUL synthetic requests are demoted; a 4xx or 5xx stays at
// WARN/ERROR, and counts stay exact.
//
// Does NOT log query parameters or request bodies: they may carry API keys or PII.
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
				"path", boundedLogField(r.URL.Path, maxLoggedPathLen),
				"status", rec.status,
				"bytes", rec.bytes,
				"latency_ms", float64(latency.Microseconds()) / 1000.0,
				"request_id", RequestIDFrom(r),
				"remote_ip", remote,
				"user_agent", boundedLogField(r.UserAgent(), maxLoggedUserAgentLen),
			}

			// A slow request additionally carries its query SHAPE. On
			// this API the query string is what decides the query plan —
			// `/v1/assets` is one route and many plans, selected by limit,
			// order_by and cursor — so `path` alone cannot tell an
			// operator which request was slow. Measured on r1:
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
			case obs.IsSyntheticRequest(r):
				logger.Debug("http request", attrs...)
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
// connection in production.
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
