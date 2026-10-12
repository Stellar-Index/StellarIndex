package middleware

import (
	"context"
	"net/http"
	"time"
)

// RequestTimeout bounds every non-streaming request's context to d as a backstop for handlers lacking their own DB timeout, since
// WriteTimeout does not cancel queries; d <= 0 disables it and SSE routes are exempt.
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

// streamingPaths is the exact set of SSE routes, not a `/stream` suffix test a caller could forge via a wildcard route.
// A new SSE route must be added here; TestRequestTimeout_StreamExemptionCannotBeForged fails until it is.
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
