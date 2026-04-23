// Package middleware has the HTTP middleware the v1 API Server wraps
// its mux in. Order (outermost first, per docs/reference/api-design.md):
//
//	RequestID → HTTPMetrics → Logger → Recoverer → CORS → RateLimit
//
// Each middleware is a tiny file. They're composable via [Chain]
// which wraps them innermost-last so the request-path order matches
// the declaration order.
//
// # Request context keys
//
// Middleware inject values into the request context via the keys in
// [context_keys.go]. Handlers read them via [FromRequest] accessors;
// never reach into the context bag directly.
//
// # Deliberately small
//
// This package does NOT wrap a router and does NOT implement auth.
// CORS support is intentionally minimal — exact-match origin
// allow-list plus wildcard, no dynamic origin reflection beyond
// that. Callers wanting per-route policies or pattern matching
// should reach for rs/cors instead.
package middleware
