package middleware

import (
	"context"
	"net/http"
	"strings"
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
// four SSE routes (/v1/ledger/stream, /v1/price/stream, /v1/price/tip/
// stream, /v1/observations/stream) all use the `/stream` path suffix,
// which is the codebase convention this middleware keys on.
func RequestTimeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d <= 0 || isStreamingPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// isStreamingPath reports whether p is one of the long-lived SSE
// endpoints, identified by the `/stream` path suffix convention. Kept
// suffix-based (not an exact allow-list) so a new SSE route inherits the
// exclusion without a second edit here — every SSE endpoint in this API
// already follows the suffix convention.
func isStreamingPath(p string) bool {
	return strings.HasSuffix(p, "/stream")
}
