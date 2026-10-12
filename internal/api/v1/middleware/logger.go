package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// SlowRequestThreshold is the latency at or above which a request also logs its query shape and `slow=true`; it equals the published p99
// target and must stay rare (~0.2% of requests).
const SlowRequestThreshold = 500 * time.Millisecond

// maxLoggedPathLen and maxLoggedUserAgentLen bound two caller-controlled fields per log line (net/http's ~1 MB header limit is not a
// log-safety one). The path bound is generous: paths can carry a 56-char Stellar id plus nested segments.
const (
	maxLoggedPathLen      = 512
	maxLoggedUserAgentLen = 256
)

// Logger emits one structured entry per request (5xx ERROR, 4xx WARN, 429 skipped, successful synthetic DEBUG); remote_ip is [RemoteIP],
// never the caller-written first X-Forwarded-For hop, and query parameters and bodies are never logged.
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

			// A slow request also carries its query SHAPE because the query string picks the plan,
			// so `path` alone cannot say which request was slow. Values are allow-listed, never
			// raw (see QueryShape), and only attached above the threshold to limit journal volume.
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

// statusRecorder wraps an http.ResponseWriter to capture status and byte count. No h2/flusher/hijacker pass-through yet;
// re-evaluate when SSE needs Flusher.
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

// Unwrap lets http.NewResponseController reach SetWriteDeadline, Flush and Hijack; without it SSE handlers cannot clear the global
// WriteTimeout.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Flush preserves http.Flusher for SSE endpoints; without it, wrapping breaks chunked streaming.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
