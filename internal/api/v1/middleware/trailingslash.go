package middleware

import (
	"net/http"
	"strings"
)

// TrailingSlashRedirect 308-redirects any non-root request whose path ends
// with `/` to the same path without it (query preserved).
//
// Why: v1 routes are registered without a trailing slash and Go's ServeMux
// treats `/v1/assets/` as a different path that 404s, yet many clients append
// one (curl typos, axios baseURL joins, OpenAPI generators). One 308 hop lands
// them on the live handler.
//
// 308 rather than 301/302 preserves method and body, so POST/DELETE do not
// degrade to GET. The redirect is method-agnostic. The root `/` is exempt, as
// is any path that is itself a registered exact-match index route (a mux
// pattern ending in `/{$}`, e.g. `GET /errors/{$}`); see muxMatcher.
//
// Sits OUTSIDE the mux so the redirect precedes the mux's 404, INSIDE Logger so
// the redirect is logged, and OUTSIDE CaptureRoute so no route pattern is
// recorded for it.
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
				// Refuse to emit a Location that isn't a clean
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
				http.Redirect(w, r, target, http.StatusPermanentRedirect) //nolint:gosec // G710: target passed isCleanSameOriginPath, a same-origin path
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
// URL to whatever host follows), no backslash (some browsers treat
// '\' as equivalent to '/', so "/\evil.com" is the same bypass spelled
// differently) and no control byte (browsers strip tab, LF and CR before
// parsing, so "/\t/evil.com" is "//evil.com" again).
func isCleanSameOriginPath(target string) bool {
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		return false
	}
	for i := 0; i < len(target); i++ {
		if c := target[i]; c == '\\' || c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}
