package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// Logger emits one structured log entry per request. The log is
// always at INFO level; 5xx responses are additionally logged at
// ERROR level with the same fields so dashboards can split.
//
// Fields (minimum):
//   - method, path, status, bytes, latency_ms
//   - request_id (from RequestID middleware)
//   - remote_ip (X-Forwarded-For first hop if present, else
//     r.RemoteAddr stripped of the port)
//   - user_agent
//
// Does NOT log query parameters or request bodies — they may
// carry API keys or PII. Add named fields in specific handlers
// when needed.
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

// Flush preserves http.Flusher for SSE endpoints — without this,
// wrapping breaks chunked streaming.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
