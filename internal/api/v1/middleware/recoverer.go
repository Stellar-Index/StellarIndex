package middleware

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recoverer turns a panicking handler into a 500 plus a structured log with the stack trace.
// The body is a generic RFC 9457 problem+json; the panic value and stack go only to the log,
// never to the client.
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

// writeProblem emits an RFC 9457 problem+json body for a recovered panic. It is local because
// importing v1 would create a cycle.
//
// If the handler already wrote a header, WriteHeader is a no-op and the body is appended to
// what it streamed. That is acceptable: the response was already broken, the stack is logged,
// and detecting a prior write would mean wrapping the writer on every request.
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
