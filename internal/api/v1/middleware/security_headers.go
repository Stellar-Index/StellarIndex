package middleware

import "net/http"

// SecurityHeaders sets the minimal set of HTTP security response
// headers that are meaningful for a JSON API. It does NOT try to
// replace the reverse-proxy's job — HSTS, CSP, and TLS-bound
// headers belong on the edge where the scheme is known. This
// middleware only sets headers whose value is always safe and
// whose benefit is application-layer.
//
// Set here:
//
//   - X-Content-Type-Options: nosniff — stops a client's
//     heuristic from ever treating a JSON response as HTML or
//     JavaScript. Prevents MIME-sniffing attacks where a hostile
//     value inside a response is sniffed into an executable
//     context by an overly helpful browser.
//   - Referrer-Policy: no-referrer — some API URLs ARE top-level
//     navigations whose query carries a credential
//     (`GET /v1/auth/callback?token=<magic-link>` 303s into the
//     dashboard; the signup verify link). No API response has a use
//     for sending a Referer onward.
//
// Not set (deliberate):
//
//   - Strict-Transport-Security — scheme-bound; the HAProxy
//     edge (HA plan §10) adds this with the right max-age and
//     includeSubDomains policy.
//   - Content-Security-Policy — primarily for HTML-serving
//     origins; our responses are JSON, so there's no DOM to
//     restrict.
//   - X-Frame-Options — prevents clickjacking of HTML pages;
//     a JSON response can't be framed.
//
// The edge Caddyfile must set Referrer-Policy conditionally
// (`?Referrer-Policy`, Caddy's set-if-absent operator) rather than
// unconditionally: an unconditional edge value in the same header
// block as a delete op is deferred to write-time (Caddy defers the
// whole block once any field is deleted) and would overwrite this
// no-referrer with the edge's looser default. Operators behind a
// reverse proxy that respects that ordering get the same value
// twice — idempotent.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
