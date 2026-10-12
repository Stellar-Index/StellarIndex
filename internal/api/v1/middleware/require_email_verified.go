// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// RequireEmailVerified 403s a `/v1/signup` key whose `EmailVerifiedAt` is zero so an unproven email yields no usable key; wire AFTER `Auth` and `KeyPolicy` and BEFORE `RateLimit`.
func RequireEmailVerified() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			subject, ok := auth.SubjectFrom(r.Context())
			if !ok || subject.Tier == auth.TierAnonymous || subject.Tier == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !strings.HasPrefix(subject.Identifier, "signup-") {
				next.ServeHTTP(w, r)
				return
			}
			if !subject.EmailVerifiedAt.IsZero() {
				next.ServeHTTP(w, r)
				return
			}
			writeEmailUnverified(w, r)
		})
	}
}

// writeEmailUnverified emits a 403 problem+json pointing at the verify endpoint. It does not echo
// the key_id; the `signup-verify-required` keyword lets dashboard clients render a resend CTA.
func writeEmailUnverified(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{
		"type":     "https://api.stellarindex.io/errors/signup-verify-required",
		"title":    "Email verification required",
		"status":   http.StatusForbidden,
		"detail":   "this API key was minted via /v1/signup but the post-signup verification email hasn't been confirmed yet. Click the link in the email we sent, or contact support if you didn't receive it.",
		"instance": r.URL.Path,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"title":"Email verification required","status":403}`)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	// Per-key denial on a per-URL cache key — never shared-cacheable
	// (overrides the route directive CacheControl pre-set).
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-StellarIndex-Signup-Verify-Required", "true")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write(body)
}
