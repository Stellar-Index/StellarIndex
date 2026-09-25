// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	htmltemplate "html/template"
	"net/http"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// SignupVerifier is the v1 boundary for the email-ownership-
// proof flow added in F-1218 (codex audit-2026-05-12). Mirrors
// `auth.SignupVerifier` but kept in the v1 package so callers
// don't have to drag the auth package onto v1's public surface.
//
// Production wiring: `auth.RedisSignupVerifier` (SETNX +
// GETDEL on `signup:verify:<sha256(token)>`).
type SignupVerifier interface {
	Reserve(ctx context.Context, token, keyID string, ttl time.Duration) error
	Consume(ctx context.Context, token string) (string, error)
}

// SignupVerifyEmailer is the v1 boundary for sending the
// verification email on POST /v1/signup. Production wiring is
// a thin adapter around `notify.Sender` (Resend in production,
// NoopSender in dev). Kept narrow so the v1 package doesn't
// drag the full notify surface onto its public API.
//
// `verifyURL` is the absolute click-through URL the customer
// sees in the email — `https://api.example.com/v1/signup/verify
// ?token=<plaintext>`. The handler builds it from the request's
// scheme + Host so deployments don't have to plumb a separate
// base URL config; nil-safe in the handler so a Sender-less
// deployment skips the send and returns the response with
// `email_sent: false`.
type SignupVerifyEmailer interface {
	SendSignupVerification(ctx context.Context, toEmail, verifyURL string) error
}

// APIKeyEmailVerifier is the v1 boundary for flipping a Redis-
// stored API key's `EmailVerifiedAt` timestamp. Production
// wiring is `auth.RedisAPIKeyStore.MarkEmailVerified`. F-1218
// wave 45 (codex audit-2026-05-12): the `/v1/signup/verify`
// handler calls this after Consume so the optional
// `middleware.RequireEmailVerified` gate can rely on the flag.
// Nil-safe — when no marker is wired (Redis-less / SAC-style
// deployment) the verify handler still succeeds, the wire
// shape stays the same, but downstream gates can't honour the
// signal.
type APIKeyEmailVerifier interface {
	MarkEmailVerified(ctx context.Context, keyID string, at time.Time) error
}

// SignupVerifyResult is the wire shape for `POST /v1/signup/verify`
// responses. The key_id surfaces so the
// dashboard / CLI can correlate the verified key with the
// account's other metadata; no plaintext is returned (the
// original signup response carried that exactly once).
type SignupVerifyResult struct {
	Verified bool   `json:"verified"`
	KeyID    string `json:"key_id,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// signupVerifyBodyMaxBytes bounds the confirmation POST: one form field.
const signupVerifyBodyMaxBytes = 4 * 1024

// signupVerifyPageCSP locks the confirmation page down to its own inline
// style and a form that may only submit back to this origin.
const signupVerifyPageCSP = "default-src 'none'; style-src 'unsafe-inline'; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// signupVerifyPageTmpl is the confirmation page. html/template escapes the
// token (it arrives from the query string) into the hidden field.
var signupVerifyPageTmpl = htmltemplate.Must(htmltemplate.New("signupverify").Parse(
	`<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<meta name="robots" content="noindex">` +
		`<title>Confirm your email · Stellar Index</title>` +
		`<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#1a1a1a}` +
		`button{font:inherit;padding:.5em 1.2em;cursor:pointer}</style></head><body>` +
		`<h1>Confirm your email address</h1>` +
		`<p>Press the button to confirm you own this address and verify the API key issued at signup. ` +
		`The link is single-use.</p>` +
		`<form method="post" action="/v1/signup/verify">` +
		`<input type="hidden" name="token" value="{{.}}">` +
		`<button type="submit">Confirm email</button></form>` +
		`</body></html>`))

// requireSignupVerifier writes the 503 when this deployment runs no
// verification flow (Redis-less), and reports whether to continue.
func (s *Server) requireSignupVerifier(w http.ResponseWriter, r *http.Request) bool {
	if s.signupVerifier != nil {
		return true
	}
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/signup-verify-unavailable",
		"Signup verification not configured", http.StatusServiceUnavailable,
		"this deployment doesn't run the email-ownership-proof flow; the original signup response is the only proof of issuance")
	return false
}

// handleSignupVerifyPage serves `GET /v1/signup/verify?token=…`, the link
// in the verification email, as a confirmation page that changes nothing.
// Mail-security scanners (Safe Links, Mimecast, Proofpoint) fetch every
// emailed link; a GET that consumed the token let the scanner prove
// ownership of the mailbox for whoever signed up with the address. Only
// the page's POST, a deliberate press, consumes it.
func (s *Server) handleSignupVerifyPage(w http.ResponseWriter, r *http.Request) {
	if !s.requireSignupVerifier(w, r) {
		return
	}
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-token",
			"Missing token", http.StatusBadRequest,
			"the verify link requires a `token` query parameter (the value emailed to you on signup)")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", signupVerifyPageCSP)
	_ = signupVerifyPageTmpl.Execute(w, token)
}

// handleSignupVerify serves `POST /v1/signup/verify` (form field `token`).
//
// F-1218 (codex audit-2026-05-12): proves the customer owns
// the email address they signed up with by consuming the
// token the signup handler emailed them. Single-use semantics
// via Redis GETDEL — a second submit returns 404, the same
// shape as a forged token. The token is read from the form
// body only, never the query string.
//
// Surfaces:
//
//   - 200 + `{"verified":true,"key_id":"…"}` on success
//   - 400 + Problem-JSON on a missing token or unreadable body
//   - 404 + Problem-JSON on unknown / consumed / expired token
//   - 503 + Problem-JSON when no SignupVerifier is configured
//     (Redis-less deployment)
func (s *Server) handleSignupVerify(w http.ResponseWriter, r *http.Request) {
	if !s.requireSignupVerifier(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, signupVerifyBodyMaxBytes)
	if err := r.ParseForm(); err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-body",
			"Invalid body", http.StatusBadRequest,
			"the confirmation must be an application/x-www-form-urlencoded body of at most 4 KiB")
		return
	}
	token := strings.TrimSpace(r.PostForm.Get("token"))
	if token == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-token",
			"Missing token", http.StatusBadRequest,
			"the confirmation requires a `token` form field (the value from the emailed link)")
		return
	}
	keyID, err := s.signupVerifier.Consume(r.Context(), token)
	if err != nil {
		if errors.Is(err, auth.ErrSignupVerifyNotFound) {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/signup-verify-not-found",
				"Verification token not found", http.StatusNotFound,
				"the token is unknown, has already been consumed, or has expired; sign up again to receive a fresh link")
			return
		}
		s.logger.Error("signup verify: store error", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError,
			"verification failed; try again in a moment")
		return
	}
	// F-1218 wave 45 (codex audit-2026-05-12): flip the
	// `EmailVerifiedAt` timestamp on the key record so the
	// optional `RequireEmailVerified` middleware can let this
	// caller through on subsequent requests. Best-effort —
	// a marker failure logs at warn but doesn't fail the
	// verify response (the token has already been consumed;
	// surfacing 500 here would leave the customer stuck).
	if s.apiKeyEmailVerifier != nil {
		if err := s.apiKeyEmailVerifier.MarkEmailVerified(r.Context(), keyID, time.Time{}); err != nil {
			s.logger.Warn("signup verify: MarkEmailVerified failed; gate stays unsatisfied for this key",
				"err", err, "key_id", keyID)
		}
	}
	writeJSON(w, SignupVerifyResult{
		Verified: true,
		KeyID:    keyID,
		Detail:   "email ownership confirmed; the API key minted at signup is now flagged as verified",
	}, Flags{})
}
