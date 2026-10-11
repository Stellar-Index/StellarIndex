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

// AllowsCredentialed reports whether origin is explicitly allow-listed for CREDENTIALED
// cross-origin access, the only kind that may drive a cookie-authenticated dashboard write.
//
// The `*` wildcard deliberately does NOT satisfy this: it means a public read API, not that any
// site may act for a logged-in customer. [CORS] likewise refuses wildcard plus credentials.
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

// RequireSameSiteWrite rejects state-changing requests that did not originate from this
// deployment's own site or an operator-allow-listed origin (OWASP "Verifying Origin With
// Standard Headers").
//
// `SameSite=Lax` alone is not enough: it is site-level, so it ignores a sibling origin under the
// same registrable domain (any `*.stellarindex.io` host) and unauthenticated routes that MINT a
// session (`/v1/auth/login`, `/v1/auth/verify-code`: login CSRF).
//
// Policy: safe methods pass. Otherwise `Origin` is used, else the origin derived from `Referer`;
// `Origin: null` is NOT trusted and falls through to Referer. Neither present: reject
// (fail-closed), safe because guarded routes are browser-only and cookie-authenticated
// (programmatic clients use Bearer, where this is not mounted). A same-origin or
// [OriginPolicy.AllowsCredentialed] origin passes; anything else gets 403 problem+json.
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

// requestOrigin reconstructs this deployment's own origin (scheme + host) as a browser would.
//
// `X-Forwarded-Proto` is honoured because TLS terminates at Caddy, so r.TLS is nil for genuine
// https. Spoofing it gains nothing: a browser sets `Origin` itself, and a client able to forge
// both headers needs no CSRF to use the victim's cookie.
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
