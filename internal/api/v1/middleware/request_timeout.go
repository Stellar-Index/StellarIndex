package middleware

import (
	"context"
	"net/http"
	"time"
)

// RequestTimeout returns middleware that bounds every non-streaming
// request's context to d, so EVERY handler inherits a deadline even
// when it forgets to wrap its own DB/ClickHouse read.
//
// This is the durable chokepoint behind C3-1/C3-2/P1 (audit-2026-07-16):
// the explorer + rate-endpoint handlers passed raw r.Context() to
// expensive lake reads with no per-request timeout, so a handful of slow
// unauthenticated requests could hold the shared 8-connection ClickHouse
// pool open indefinitely (the server WriteTimeout does NOT cancel an
// in-flight query). A request-scoped deadline lets those reads observe
// ctx cancellation and release their pool connection. Per-handler
// context.WithTimeout wrappers (8s on the hot reads) still layer UNDER
// this — they're tighter, so they fire first; this is the backstop for
// every path that lacks one.
//
// d SHOULD be longer than the per-read timeouts (8s) so a per-read
// deadline surfaces its own, more specific error before this blanket
// one; and shorter than the http.Server WriteTimeout so the deadline is
// meaningful. d <= 0 disables the middleware (no deadline injected).
//
// Streaming (SSE) endpoints are EXCLUDED: they are long-lived by design
// and own their lifecycle through r.Context() cancellation on client
// disconnect. A request deadline would sever the stream mid-flight. The
// exempt routes are the exact set in streamingPaths.
func RequestTimeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// EscapedPath, not Path. The mux routes on the escaped
			// form, but r.URL.Path is DECODED — so
			// /v1/assets/native%2Fstream decodes to a path ending
			// "/stream" while still routing to the ordinary
			// /v1/assets/{asset_id} handler. Keying the exemption on the
			// decoded form let any trailing-wildcard route forge it and
			// run with NO deadline at all (wave-D UNAUTH-DOS-4).
			//
			// Deliberately not "move the check after the mux": that would
			// mean moving RequestTimeout INSIDE the mux, reopening the
			// C3-102 pre-handler-stack gap this middleware's placement
			// exists to close.
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

// streamingPaths is the exact set of SSE routes. An exact set rather than
// a `/stream` suffix test: any route ending in a wildcard would otherwise
// match when a caller sets that wildcard to "stream". A new SSE route must
// be added here; TestRequestTimeout_StreamExemptionCannotBeForged fails
// until it is.
var streamingPaths = map[string]bool{
	"/v1/ledger/stream":       true,
	"/v1/price/stream":        true,
	"/v1/price/tip/stream":    true,
	"/v1/observations/stream": true,
}

// isStreamingPath reports whether p is one of the long-lived SSE endpoints.
//
// MUST be called with the ESCAPED path. The escaped form is what the mux
// itself routes on, so the exemption and the router cannot disagree about
// what a request is.
func isStreamingPath(p string) bool {
	return streamingPaths[p]
}
