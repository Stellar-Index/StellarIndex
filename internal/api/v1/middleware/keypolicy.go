// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// KeyPolicy enforces per-key IP allowlist, Referer allowlist, scopes and permissions AFTER [Auth] and before [RateLimit]; anonymous
// subjects bypass, every other tier (operator included) is bound by its own gates.
func KeyPolicy() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			subject, ok := auth.SubjectFrom(r.Context())
			if !ok || subject.Tier == auth.TierAnonymous {
				next.ServeHTTP(w, r)
				return
			}
			if slug, err := checkKeyPolicy(r, subject); err != nil {
				writeKeyPolicyDenied(w, r, slug, err.Error())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// checkKeyPolicy runs the per-key gates in order (IP, Referer, scopes, permissions) and returns the first failure's slug and error.
// Operator-tier subjects get no early return: a key narrowed at mint must stay bound by its scopes and permissions.
func checkKeyPolicy(r *http.Request, subject auth.Subject) (string, error) {
	if err := checkIPAllowlist(r, subject.IPAllowlist); err != nil {
		return "ip-not-allowed", err
	}
	if err := checkRefererAllowlist(r, subject.RefererAllowlist); err != nil {
		return "referer-not-allowed", err
	}
	if err := checkScopes(r, subject); err != nil {
		return "scope-denied", err
	}
	if err := checkPermissions(r, subject); err != nil {
		return "permission-denied", err
	}
	return "", nil
}

func checkIPAllowlist(r *http.Request, allow []netip.Prefix) error {
	if len(allow) == 0 {
		return nil
	}
	raw := RemoteIP(r)
	if raw == "" {
		return fmt.Errorf("could not resolve client IP")
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return fmt.Errorf("client IP %q is malformed", raw)
	}
	for _, prefix := range allow {
		if prefix.Contains(addr) {
			return nil
		}
	}
	return fmt.Errorf("client IP %s not in this key's allowlist", addr.String())
}

func checkRefererAllowlist(r *http.Request, allow []string) error {
	if len(allow) == 0 {
		return nil
	}
	raw := r.Header.Get("Referer")
	if raw == "" {
		return fmt.Errorf("referer header is required for this key but was missing")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("referer header %q is malformed", raw)
	}
	host := strings.ToLower(u.Host)
	for _, entry := range allow {
		if strings.EqualFold(strings.TrimSpace(entry), host) {
			return nil
		}
	}
	return fmt.Errorf("referer host %q not in this key's allowlist", host)
}

// checkScopes enforces the coarse route-family scopes; an empty list passes (opt-in at mint). It runs before permissions,
// so scopes bound the family and permissions bound endpoints inside it.
func checkScopes(r *http.Request, subject auth.Subject) error {
	if len(subject.Scopes) == 0 {
		return nil
	}
	need := auth.RequiredScope(r.URL.Path)
	if subject.HasScope(need) {
		return nil
	}
	return fmt.Errorf("this key's scopes (%s) do not include %q, required for %s %s",
		strings.Join(subject.Scopes, ", "), need, r.Method, r.URL.Path)
}

// ClampMintScopes is the scope half of [auth.ClampToMinter] as a problem string for handlers; not the enforcement point,
// RedisAPIKeyStore.Create re-applies the clamp for any MintedBy request.
func ClampMintScopes(caller auth.Subject, requested []string) ([]string, string) {
	scopes, err := auth.ClampToMinter(caller, requested, 0)
	if err != nil {
		return nil, strings.TrimPrefix(err.Error(), auth.ErrMintExceedsCaller.Error()+": ")
	}
	return scopes, ""
}

func checkPermissions(r *http.Request, subject auth.Subject) error {
	if len(subject.DenyPermissions) > 0 && permissionMatches(r, subject.DenyPermissions) {
		return fmt.Errorf("this key is denied access to %s %s", r.Method, r.URL.Path)
	}
	if subject.AllowAllPermissions {
		return nil
	}
	if len(subject.AllowPermissions) == 0 {
		// Closed posture: no allow entries + AllowAllPermissions=false
		// means "no endpoints permitted". This shouldn't happen for
		// keys minted via the dashboard (the UI defaults to All:true);
		// guard against a future revoked-but-not-disabled key shape.
		return fmt.Errorf("this key has no permission entries; contact account owner")
	}
	if permissionMatches(r, subject.AllowPermissions) {
		return nil
	}
	return fmt.Errorf("this key is not permitted on %s %s", r.Method, r.URL.Path)
}

func permissionMatches(r *http.Request, entries []auth.SubjectPermissionEntry) bool {
	exact := r.Method + " " + r.URL.Path
	for _, e := range entries {
		if e.Endpoint != "" && e.Endpoint == exact {
			return true
		}
		if e.EndpointPrefix != "" && prefixMatchesPathSegment(r.URL.Path, e.EndpointPrefix) {
			return true
		}
	}
	return false
}

// prefixMatchesPathSegment reports whether path is prefix or lies under it as a path segment ("/v1/admin" matches "/v1/admin/keys",
// not "/v1/administer"), so a permission entry cannot leak onto a sibling route sharing a string prefix.
func prefixMatchesPathSegment(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	if strings.HasSuffix(prefix, "/") {
		return true
	}
	return len(path) == len(prefix) || path[len(prefix)] == '/'
}

// writeKeyPolicyDenied emits an RFC 9457 problem+json 403 with a reason-specific `type` URI, so dashboard clients can tell an IP-allowlist
// denial from a permission denial without parsing the detail string.
func writeKeyPolicyDenied(w http.ResponseWriter, r *http.Request, slug, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	// Override the route directive CacheControl pre-set — a 403 on a
	// publicly-cacheable route must never be shared-cacheable (the
	// denial is per-key/per-IP, the cache key is per-URL).
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	body, _ := json.Marshal(map[string]any{
		"type":     "https://api.stellarindex.io/errors/" + slug,
		"title":    "Forbidden",
		"status":   http.StatusForbidden,
		"detail":   detail,
		"instance": r.URL.Path,
	})
	_, _ = w.Write(body)
}
