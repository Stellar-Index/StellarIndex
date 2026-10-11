// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// RequireEmailVerified returns a [Middleware] that 403s API-key callers
// whose `EmailVerifiedAt` is still zero: the customer signed up via `POST
// /v1/signup` but hasn't clicked the verification link. Without it, a
// valid-looking email string yields a usable plaintext Starter key with no
// ownership proof; with it, the key stays 403'd until the link is clicked
// (`/v1/signup/verify` flips the flag).
//
//   - Anonymous Subjects pass (no key to gate).
//   - Subjects whose `Identifier` does NOT start with `signup-` pass:
//     operator-minted (`customer-acme-corp`) and dashboard-minted
//     (`acct:<slug>`) keys never went through the email-link flow. Only
//     `/v1/signup`-issued keys are checked.
//   - `EmailVerifiedAt.IsZero()` AND a `signup-` prefix → 403 +
//     Problem-JSON pointing at the verify endpoint.
//
// The api binary mounts this only when
// `cfg.API.SignupRequireEmailVerification` is true (the default). Wire
// AFTER `Auth` and `KeyPolicy`, BEFORE `RateLimit`, so a
// verification-failed request doesn't also spend a rate-limit token.
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

// writeEmailUnverified emits a 403 Problem-JSON pointing the
// customer at the verify endpoint. The body intentionally
// does NOT echo back the key_id (operators distinguish
// individual customer flows via /v1/account/me); the keyword
// `signup-verify-required` lets dashboard clients render a
// resend-link CTA.
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
