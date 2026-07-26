// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// OriginPolicy is the operator's configured cross-origin allow-list,
// published on the request context by [CORS] so downstream guards can
// reuse it rather than keeping a second, drift-prone copy.
type OriginPolicy struct {
	allowed     map[string]bool
	credentials bool
}

// AllowsCredentialed reports whether `origin` is one the operator
// explicitly allow-listed for CREDENTIALED cross-origin access — the
// only kind that can drive a cookie-authenticated dashboard write.
//
// The `*` wildcard deliberately does NOT satisfy this. A wildcard
// means "this is a public read API"; it cannot mean "any site on the
// internet may act on a logged-in customer's behalf" — and
// [CORS] already refuses to pair the wildcard with credentials, so a
// wildcard deployment never carries cookies cross-origin anyway.
func (p *OriginPolicy) AllowsCredentialed(origin string) bool {
	if p == nil || origin == "" || !p.credentials {
		return false
	}
	return p.allowed[origin]
}

type originPolicyKey struct{}

func withOriginPolicy(ctx context.Context, p *OriginPolicy) context.Context {
	return context.WithValue(ctx, originPolicyKey{}, p)
}

// OriginPolicyFrom reads the CORS allow-list off r's context.
// ok=false when [CORS] middleware wasn't applied at all.
func OriginPolicyFrom(ctx context.Context) (*OriginPolicy, bool) {
	p, ok := ctx.Value(originPolicyKey{}).(*OriginPolicy)
	return p, ok
}

// RequireSameSiteWrite rejects state-changing requests that did not
// originate from this deployment's own site or from an operator-
// allow-listed origin, per the OWASP CSRF Prevention Cheat Sheet's
// "Verifying Origin With Standard Headers" defence.
//
// C3-031 / C3-057 (audit-2026-07-23): the dashboard's cookie-authed
// mutation surface had NO CSRF defence beyond `SameSite=Lax` on the
// session cookie. Lax stops a cross-SITE POST from carrying the
// cookie, which is real protection — but it is a *site*-level
// control, so it does nothing about a sibling origin under the same
// registrable domain (any `*.stellarindex.io` host, e.g. a
// customer-content or preview subdomain, is same-site), and nothing
// about the un-authenticated state-changing routes that MINT a
// session rather than consume one (`/v1/auth/login`,
// `/v1/auth/verify-code` — the login-CSRF pair to C3-030). This
// guard closes both: the `Origin` must be the API's own origin or
// one the operator explicitly allow-listed for CORS.
//
// Policy, in order:
//
//   - Safe methods (GET / HEAD / OPTIONS) pass untouched. They must
//     not be state-changing in the first place; CORS preflight also
//     short-circuits above this.
//   - `Origin` is used when present; otherwise the origin is derived
//     from `Referer`. `Origin: null` (sandboxed iframe, some
//     redirect chains) is NOT trusted and falls through to Referer.
//   - Neither header present → **reject**. This is the fail-closed
//     arm the cheat sheet recommends, and it is safe on this surface
//     specifically because every route it guards is a browser-only,
//     cookie-authenticated dashboard route — programmatic clients
//     use `Authorization: Bearer` on the API surface, which this
//     guard is deliberately not mounted on.
//   - Same origin as the request itself → pass.
//   - Explicitly allow-listed for credentialed CORS → pass. That is
//     the `stellarindex.io` explorer calling `api.stellarindex.io`:
//     the operator already has to allow-list it for the credentialed
//     fetch to work at all, so there is exactly one list to keep. See
//     [OriginPolicy.AllowsCredentialed] for why the `*` wildcard is
//     not accepted here.
//   - Anything else → 403 problem+json.
func RequireSameSiteWrite(logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			origin, reason := writeOriginOf(r)
			if reason == "" {
				next.ServeHTTP(w, r)
				return
			}
			logger.Warn("cross-site write blocked",
				"reason", reason, "origin", origin,
				"method", r.Method, "path", r.URL.Path,
				"remote_ip", RemoteIPFrom(r))
			writeCrossSiteDenied(w, r)
		})
	}
}

// writeOriginOf resolves the origin a state-changing request came
// from and classifies it. An empty reason means "allowed"; a
// non-empty reason is the deny label used for logging.
func writeOriginOf(r *http.Request) (origin, reason string) {
	origin = r.Header.Get("Origin")
	// "null" is what a sandboxed iframe / opaque origin sends. It is
	// forgeable-adjacent and identifies nothing, so treat it as
	// absent and let Referer decide.
	if origin == "" || origin == "null" {
		origin = originFromReferer(r.Header.Get("Referer"))
	}
	switch {
	case origin == "":
		return "", "no-origin-or-referer"
	case strings.EqualFold(origin, requestOrigin(r)):
		return origin, ""
	default:
		p, _ := OriginPolicyFrom(r.Context())
		if p.AllowsCredentialed(origin) {
			return origin, ""
		}
		return origin, "origin-not-allowed"
	}
}

// requestOrigin reconstructs this deployment's own origin as the
// browser would have computed it for the request: scheme + host.
//
// `X-Forwarded-Proto` is honoured because the production topology
// terminates TLS at Caddy and forwards cleartext to :3000, so
// r.TLS is nil for genuine https traffic. Spoofing it buys an
// attacker nothing: a browser sets `Origin` itself and a client
// that can forge BOTH headers can already forge the whole request
// and doesn't need CSRF to reach the victim's cookie.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	switch {
	case r.TLS != nil:
		scheme = "https"
	case r.Header.Get("X-Forwarded-Proto") != "":
		xfp := r.Header.Get("X-Forwarded-Proto")
		if i := strings.IndexByte(xfp, ','); i >= 0 {
			xfp = xfp[:i]
		}
		scheme = strings.ToLower(strings.TrimSpace(xfp))
	}
	return scheme + "://" + r.Host
}

// originFromReferer reduces a Referer URL to its origin. Returns ""
// for anything that isn't an absolute http(s) URL.
func originFromReferer(referer string) string {
	if referer == "" {
		return ""
	}
	u, err := url.Parse(referer)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// isSafeMethod reports whether the method is read-only per RFC 9110
// §9.2.1 and therefore outside the CSRF write guard.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// writeCrossSiteDenied emits the RFC 9457 problem+json 403. The
// detail deliberately names the control so a dashboard developer
// hitting it from an un-allow-listed dev origin gets a pointer
// rather than a mystery 403.
func writeCrossSiteDenied(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/problem+json")
	// Never shared-cacheable: the verdict is per-request-header, the
	// cache key is per-URL. Same reasoning as writeKeyPolicyDenied.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	body, _ := json.Marshal(map[string]any{
		"type":   "https://api.stellarindex.io/errors/cross-site-request-blocked",
		"title":  "Forbidden",
		"status": http.StatusForbidden,
		"detail": "state-changing dashboard requests must carry an Origin (or Referer) " +
			"matching this API or an allow-listed site",
		"instance": r.URL.Path,
	})
	_, _ = w.Write(body)
}
