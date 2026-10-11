package middleware

import (
	"context"
	"net/http"
	"time"
)

// RequestTimeout bounds every non-streaming request's context to d, as a backstop for handlers
// that forget their own DB/ClickHouse timeout. The server WriteTimeout does not cancel an
// in-flight query, so a few slow unauthenticated requests could otherwise pin the small
// ClickHouse pools indefinitely. Per-handler timeouts (8s) layer under this and fire first.
//
// d SHOULD exceed the per-read timeouts and stay below the http.Server WriteTimeout; d <= 0
// disables it. SSE routes (streamingPaths) are exempt because a deadline would sever the stream.
func RequestTimeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// EscapedPath, not Path: the mux routes on the escaped form, while Path is decoded,
			// so /v1/assets/native%2Fstream would forge the "/stream" exemption and run with
			// no deadline. The check cannot move after the mux, which would leave the
			// pre-handler stack without a deadline.
			if d <= 0 || isStreamingPath(r.URL.EscapedPath()) {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// streamingPaths is the exact set of SSE routes, not a `/stream` suffix test, which a caller
// could forge through any trailing-wildcard route. A new SSE route must be added here;
// TestRequestTimeout_StreamExemptionCannotBeForged fails until it is.
var streamingPaths = map[string]bool{
	"/v1/ledger/stream":       true,
	"/v1/price/stream":        true,
	"/v1/price/tip/stream":    true,
	"/v1/observations/stream": true,
}

// isStreamingPath reports whether p is a long-lived SSE endpoint. p MUST be the ESCAPED path,
// the form the mux routes on, so the exemption and the router cannot disagree.
func isStreamingPath(p string) bool {
	return streamingPaths[p]
}
