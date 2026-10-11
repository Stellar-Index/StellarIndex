package middleware

import "net/http"

// SecurityHeaders sets the minimal HTTP security response headers
// meaningful for a JSON API. HSTS, CSP and TLS-bound headers belong on the
// reverse-proxy edge where the scheme is known; this only sets headers
// whose value is always safe.
//
//   - X-Content-Type-Options: nosniff — stops a hostile value inside a
//     JSON response being MIME-sniffed into an executable context.
//   - Referrer-Policy: no-referrer — some API URLs ARE top-level
//     navigations whose query carries a credential (`GET
//     /v1/auth/callback?token=<magic-link>`; the signup verify link),
//     and no API response needs to send a Referer onward.
//
// Not set (deliberate): Strict-Transport-Security (scheme-bound; the
// HAProxy edge, HA plan §10, adds it), Content-Security-Policy (JSON, no
// DOM to restrict) and X-Frame-Options (a JSON response can't be framed).
//
// The edge Caddyfile must set Referrer-Policy set-if-absent
// (`?Referrer-Policy`): Caddy defers a header block to write-time once any
// field in it is deleted, so an unconditional edge value would overwrite
// this no-referrer.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
