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

// KeyPolicy returns a Middleware that enforces the per-key policy
// fields the dashboard exposes — IP allowlist, Referer allowlist,
// and per-endpoint permissions. Runs AFTER [Auth] so the
// authenticated Subject is on the request context; before
// [RateLimit] so policy-rejected requests never spend a rate-limit
// token.
//
// F-1226 (codex audit-2026-05-12): the dashboard let customers
// configure these fields but no middleware enforced them at
// request time. A 403 reply here is the same shape as Auth's
// problem+json; the body carries which control rejected (ip /
// referer / permission) so dashboard users can debug their own
// configuration.
//
// Behaviour per check:
//
//   - IPAllowlist empty: skip the IP gate (no opt-in).
//     Non-empty: the request's resolved client IP (via
//     [RemoteIP] which honours the trusted-proxy CIDRs) must
//     fall in at least one prefix.
//   - RefererAllowlist empty: skip. Non-empty: the request's
//     Referer header must be present and its host must exactly
//     match one entry (case-insensitive). Empty / missing
//     Referer is rejected — the customer asked for the gate.
//   - Scopes empty: skip the scope gate (full access — the
//     pre-scopes posture every legacy key keeps). Non-empty: the
//     request path's route family (auth.RequiredScope) must be in
//     the key's scope list.
//   - AllowAllPermissions=true: permission gate passes unless
//     a deny entry matches. False: at least one allow entry
//     must match, and no deny entry may.
//
// Anonymous subjects (Tier=anonymous) bypass every check — they
// don't carry per-key policy. Every other tier, INCLUDING operator,
// is subject to its OWN configured gates: a default operator key
// (empty Scopes, AllowAllPermissions=true) passes the scope +
// permission gates freely, while an operator key deliberately narrowed
// at mint (a scope subset or a restricted permission list) is confined
// to what it was granted. Admin-endpoint ACCESS is gated on
// Tier==Operator in the handlers, not on scope, so narrowing a staff
// key's scopes bounds its data reach without locking it out of admin.
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

// checkKeyPolicy runs the per-key gates in order — IP → Referer →
// scopes → permissions — returning the reason slug + error of the
// first failure ("" + nil when all pass). Operator-tier subjects run
// through the SAME gates as every other tier: a default operator key
// (empty Scopes, AllowAllPermissions=true) passes scopes + permissions
// freely, but an operator key deliberately narrowed at mint is confined
// to what it was granted. There is no operator early-return — an
// operator key that carries an explicit scope subset or permission list
// must actually be bound by it (pre-fix it silently bypassed both,
// re-opening the very narrowing the operator configured).
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

// checkScopes enforces the coarse route-family capability scopes.
// Keys with an empty scope list pass unconditionally (back-compat:
// scopes are opt-in at mint time). Runs before the fine-grained
// permission entries so the two layers compose: scopes bound the
// family, permissions bound individual endpoints inside it.
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

// ClampMintScopes enforces the delegation invariant that a key may
// only mint a child no more privileged than itself: the child's
// capability scopes must be a subset of the parent's. It is the
// single chokepoint every mint path funnels through so "a credential
// can never mint a more-privileged credential than itself" holds by
// construction, mirroring the same subset semantics [Subject.HasScope]
// enforces at request time.
//
// caller is the authenticated minting subject; requested is that
// caller's scope request, already validated against the vocabulary
// and de-duplicated. It returns the effective scope set to persist,
// or a non-empty problem string when the caller asked for a scope it
// does not itself hold.
//
// Rules:
//   - A full-access parent (empty scope list — the pre-scopes posture,
//     every capability) delegates freely, including minting another
//     full-access key.
//   - A scoped parent with no explicit request inherits the parent's
//     OWN scopes rather than defaulting to full access — a scoped key
//     may not mint an empty (full-access) child.
//   - A scoped parent with an explicit request must have every
//     requested scope in its own set (via [Subject.HasScope], so a "*"
//     wildcard parent still delegates freely); any scope it lacks is
//     rejected rather than silently dropped.
func ClampMintScopes(caller auth.Subject, requested []string) ([]string, string) {
	if len(caller.Scopes) == 0 {
		return requested, ""
	}
	if len(requested) == 0 {
		out := make([]string, len(caller.Scopes))
		copy(out, caller.Scopes)
		return out, ""
	}
	for _, s := range requested {
		if !caller.HasScope(s) {
			return nil, fmt.Sprintf(
				"scope %q exceeds this key's own scopes (%s) — a key may only mint a child with a subset of its scopes",
				s, strings.Join(caller.Scopes, ", "))
		}
	}
	return requested, ""
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
		if e.EndpointPrefix != "" && strings.HasPrefix(r.URL.Path, e.EndpointPrefix) {
			return true
		}
	}
	return false
}

// writeKeyPolicyDenied emits an RFC 9457 problem+json 403 with a
// reason-specific `type` URI so dashboard clients can render
// "your IP isn't in this key's allowlist" vs "you can't call this
// endpoint with this key" without parsing the detail string.
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
