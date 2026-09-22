package middleware

import (
	"net/http"
	"strings"
)

// TrailingSlashRedirect 308-redirects any non-root request whose
// path ends with `/` to the same path with the trailing slash
// stripped (query string preserved).
//
// Why: every v1 route is registered without a trailing slash
// (`GET /v1/assets`, `GET /v1/assets/{slug}`, …) and Go's net/http
// ServeMux treats `/v1/assets/` as a *different* path that 404s.
// Many client libraries auto-append a trailing slash by default
// (curl users mistype, axios with `baseURL: '.../v1/'` joins
// awkwardly, OpenAPI generators emit either form depending on
// codegen flags). Without this middleware those clients hit a
// dead 404 even though the resource exists. With it they take
// a single 308 hop and land on the live handler.
//
// 308 (rather than 301/302) preserves the request method and
// body so a POST/DELETE doesn't silently degrade to GET on the
// hop. Browsers and standard clients all honour 308 since 2017.
//
// The redirect is method-agnostic — applies to GET, HEAD, POST,
// DELETE etc. The root path `/` is exempt (it would redirect to
// the empty string), and so is any path that is itself a
// registered exact-match index route (a mux pattern ending in
// `/{$}`, e.g. `GET /errors/{$}`) — see muxMatcher below.
//
// Sits OUTSIDE the mux so the redirect happens before the mux's
// 404 fires. Sits INSIDE Logger so the redirect itself is
// logged, and OUTSIDE the mux's CaptureRoute so it doesn't try
// to record a route pattern for the redirect response.
func TrailingSlashRedirect(mux muxMatcher) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if len(p) > 1 && p[len(p)-1] == '/' {
				// A path the mux itself resolves via an exact-match
				// index pattern (`/{$}`) is canonical AT the trailing
				// slash, not a client typo of the no-slash form. Without
				// this exemption, stripping it here loops forever: the
				// mux's own automatic subtree redirect sends the
				// stripped no-slash request straight back to the
				// trailing-slash form (e.g. `/errors` -> mux 301 ->
				// `/errors/` -> this middleware's 308 -> `/errors` -> …).
				if _, pattern := mux.Handler(r); strings.HasSuffix(pattern, "/{$}") {
					next.ServeHTTP(w, r)
					return
				}
				target := p[:len(p)-1]
				// SEC-16: refuse to emit a Location that isn't a clean
				// same-origin absolute path. A request path of "//evil.com/"
				// strips to "//evil.com" here — a PROTOCOL-RELATIVE URL that
				// browsers resolve as an absolute redirect to evil.com, an
				// unauthenticated open redirect on the API origin (this
				// middleware sits OUTSIDE the mux, so it runs before
				// net/http's own ServeMux path-cleaning would otherwise catch
				// the double slash). Falling through to next lets the mux
				// either 404 or apply its own safe same-origin path cleanup.
				if !isCleanSameOriginPath(target) {
					next.ServeHTTP(w, r)
					return
				}
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusPermanentRedirect)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// muxMatcher is satisfied by *http.ServeMux (Go's enhanced routing).
// Narrowed to the one method TrailingSlashRedirect needs so tests can
// fake it without standing up a real mux.
type muxMatcher interface {
	Handler(r *http.Request) (http.Handler, string)
}

// isCleanSameOriginPath reports whether target is safe to redirect a
// caller to: a same-origin absolute path with exactly one leading '/'
// (not "//…", which a browser resolves as a protocol-relative absolute
// URL to whatever host follows) and no backslash (some browsers treat
// '\' as equivalent to '/', so "/\evil.com" is the same bypass spelled
// differently).
func isCleanSameOriginPath(target string) bool {
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		return false
	}
	return !strings.Contains(target, "\\")
}
