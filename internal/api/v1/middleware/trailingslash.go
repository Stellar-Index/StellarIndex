package middleware

import (
	"net/http"
	"strings"
)

// TrailingSlashRedirect 308-redirects a non-root path ending in `/` to the slashless path (query preserved); sits OUTSIDE the mux and CaptureRoute, INSIDE Logger.
func TrailingSlashRedirect(mux muxMatcher) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if len(p) > 1 && p[len(p)-1] == '/' {
				// An exact-match index pattern (`/{$}`) is canonical AT the trailing slash.
				// Stripping it would loop forever against the mux's own subtree redirect
				// (`/errors` -> 301 -> `/errors/` -> 308 -> `/errors`).
				if _, pattern := mux.Handler(r); strings.HasSuffix(pattern, "/{$}") {
					next.ServeHTTP(w, r)
					return
				}
				target := p[:len(p)-1]
				// Never emit a Location that is not a clean same-origin path: "//evil.com/"
				// strips to the protocol-relative "//evil.com", an open redirect on the API
				// origin. This runs before the mux's own path cleaning, so fall through and
				// let the mux 404 or clean it safely.
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

// isCleanSameOriginPath reports whether target is safe to redirect to: exactly one leading '/'
// (not protocol-relative "//"), no backslash (browsers treat '\' as '/'), and no control byte
// (browsers strip tab, LF and CR, so "/\t/evil.com" becomes "//evil.com").
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
