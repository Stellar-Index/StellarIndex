package middleware

import "net/http"

// Middleware wraps an http.Handler with some pre/post behaviour.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware to h; the first entry in mws is the OUTERMOST wrapper, so order matches the request flow.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
