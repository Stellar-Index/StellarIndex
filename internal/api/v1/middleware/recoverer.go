package middleware

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recoverer turns a panicking handler into a 500 with a stack-trace log; the body is generic problem+json and the panic value and stack
// never reach the client.
func Recoverer(logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is the stdlib signal for
				// "abort but don't log as a panic" — honour it.
				if recErr, ok := rec.(error); ok && errors.Is(recErr, http.ErrAbortHandler) {
					panic(rec)
				}

				logger.Error("handler panic",
					"request_id", RequestIDFrom(r),
					"method", r.Method,
					"path", r.URL.Path,
					"remote_ip", RemoteIPFrom(r),
					"panic", rec,
					"stack", string(debug.Stack()),
				)

				writeProblem(w, r, rec)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// writeProblem emits an RFC 9457 body for a recovered panic (local: importing v1 would cycle). If the handler already wrote a header,
// the body is appended to what it streamed; acceptable, since the stack is logged and detecting a prior write costs a wrapper per request.
func writeProblem(w http.ResponseWriter, r *http.Request, _ any) {
	w.Header().Set("Content-Type", "application/problem+json")
	// Override the cache-control middleware's per-route directive:
	// never cache a 500. Recovered panics are likely transient and
	// caching the response would replay the failure on the same
	// CDN key for the directive's lifetime.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusInternalServerError)
	body := map[string]any{
		"type":     "https://api.stellarindex.io/errors/internal",
		"title":    "Internal error",
		"status":   http.StatusInternalServerError,
		"instance": r.URL.RequestURI(),
	}
	if id := RequestIDFrom(r); id != "" {
		body["request_id"] = id
	}
	_ = json.NewEncoder(w).Encode(body)
}
