// Package dashboardauth implements the magic-link login flow
// + session middleware for the customer dashboard.
//
// Distinct from internal/auth (bearer-token API auth):
//
//   - internal/auth handles `Authorization: Bearer <key>` for
//     programmatic API requests; the Subject it produces is
//     scoped to a single API key.
//   - dashboardauth handles cookie-based dashboard sessions;
//     the Subject it produces is scoped to a User (and via
//     User.AccountID to an Account).
//
// The two surfaces will eventually meet — admin endpoints want
// to accept either a staff session OR a tier=operator API key
// — but for v1 they don't intersect: dashboard endpoints
// accept sessions only; programmatic endpoints accept keys
// only.
package dashboardauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// SessionCookieName is the HTTP cookie name used for dashboard
// sessions. Distinct from any future API-side cookies so a
// browser logged into both can't leak credentials between
// surfaces.
const SessionCookieName = "stellarindex_session"

// MagicLinkPlaintextLen — the random-bytes length we use for
// magic-link tokens. 32 bytes = 256 bits = preimage-safe;
// the hex-encoded form is what the user sees in the URL.
const MagicLinkPlaintextLen = 32

// generateMagicLinkToken returns (plaintext, sha256-hash, code).
// The plaintext is what we put in the email link;
// the hash is what we store in magic_link_tokens;
// the code is the paste-friendly 6-digit numeric variant, derived
// under `secret` (see [Generator.CodeForHash] for why it is keyed).
//
// `read` is the entropy source. crypto/rand.Read in production;
// tests inject a deterministic source.
func generateMagicLinkToken(read func([]byte) (int, error), secret []byte) (plaintext string, hash []byte, code string, err error) {
	buf := make([]byte, MagicLinkPlaintextLen)
	n, err := read(buf)
	if err != nil {
		return "", nil, "", fmt.Errorf("dashboardauth: entropy read: %w", err)
	}
	if n != MagicLinkPlaintextLen {
		return "", nil, "", fmt.Errorf("dashboardauth: short read: got %d want %d", n, MagicLinkPlaintextLen)
	}

	plaintext = hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plaintext))

	return plaintext, sum[:], codeFromHashKeyed(secret, sum[:]), nil
}

// loginCodeDomain domain-separates the code HMAC from any other use
// of the server secret, so a code can never be confused with (or
// replayed as) another derived value.
const loginCodeDomain = "stellarindex/login-code/v1|"

// codeFromHashKeyed derives the 6-digit email code from a stored
// token hash UNDER A SERVER-SIDE SECRET.
//
// History (audit-2026-08-03, aggregate+dashboardauth: "the 6-digit
// code is derivable from the stored hash"): the previous derivation
// was an unkeyed, public function of magic_link_tokens.token_hash —
// base32 of the hash's first 4 bytes mapped to digits. Anyone with a
// read of that table (SQL injection on any other surface, a stolen
// backup, a curious operator) could compute every in-flight sign-in
// code DIRECTLY — no brute force, no email access — and mint a
// session for any address they could trigger a login for via
// POST /v1/auth/verify-code. The token PLAINTEXT was never
// recoverable (preimage-safe), but the code path made that
// irrelevant: the code was equivalent to the token, and the code was
// public knowledge given the row.
//
// Now the code is HMAC-SHA256(secret, domain || token_hash) reduced
// to 6 digits. The secret lives in config/env (never in Postgres), so
// a database read alone yields nothing: without the secret the code
// is uniformly unpredictable, and the existing online-guessing bounds
// (per-token attempt cap + durable per-email lockout, C3-032) are the
// only attack surface left — unchanged UX, one derivation swapped.
//
// The uint32 % 10^6 reduction has negligible modulo bias (2^32 is
// ~4295 full cycles of 10^6; the first 967296 codes appear once more
// than the rest — a 0.02% skew, irrelevant at 10 guesses/day).
func codeFromHashKeyed(secret, hash []byte) string {
	if len(hash) < 4 {
		// Defensive: a malformed hash must not silently produce a
		// guessable constant-derived code.
		return ""
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(loginCodeDomain))
	mac.Write(hash)
	sum := mac.Sum(nil)
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum[:4])%1_000_000)
}

// HashMagicLinkPlaintext returns the sha256 hash of a
// user-supplied plaintext token. Exported so the callback
// handler can derive the hash to look up in TokenStore.
func HashMagicLinkPlaintext(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// Generator wraps the entropy source — production uses
// crypto/rand.Read; tests inject a fixed source for
// deterministic plaintexts.
type Generator struct {
	Read func([]byte) (int, error)
	// Secret keys the 6-digit code derivation (see
	// [Generator.CodeForHash]). Production wires it from the
	// STELLARINDEX_DASHBOARD_CODE_SECRET env (config
	// api.dashboard.code_secret_env); when left empty,
	// Config.validate() fills a random per-process secret so the
	// derivation is NEVER unkeyed — the trade-off being that
	// in-flight codes stop verifying across a restart (the magic
	// link is unaffected; codes live 15 minutes anyway).
	Secret []byte
}

// NewGenerator returns a production-default Generator. The caller
// (or Config.validate()) supplies Secret.
func NewGenerator() *Generator {
	return &Generator{Read: rand.Read}
}

// NewToken mints (plaintext, hash, code).
func (g *Generator) NewToken() (plaintext string, hash []byte, code string, err error) {
	return generateMagicLinkToken(g.Read, g.Secret)
}

// CodeForHash re-derives the 6-digit code for a stored token hash
// under this Generator's secret. The verify-code handler computes it
// per candidate token and constant-time compares against the
// user-supplied code — the code itself is never stored anywhere.
func (g *Generator) CodeForHash(hash []byte) string {
	return codeFromHashKeyed(g.Secret, hash)
}

// generateSessionID — 16 bytes of crypto/rand → uuid.UUID.
// Exported as a Generator method so tests can pin.
func (g *Generator) NewSessionID() (uuid.UUID, error) {
	var buf [16]byte
	n, err := g.Read(buf[:])
	if err != nil {
		return uuid.Nil, fmt.Errorf("dashboardauth: session id: %w", err)
	}
	if n != 16 {
		return uuid.Nil, fmt.Errorf("dashboardauth: short read: got %d want 16", n)
	}
	// UUID v4 — set version + variant bits per RFC 4122.
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	id, err := uuid.FromBytes(buf[:])
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// ─── Session context helpers ──────────────────────────────────────

type sessionKey struct{}

// SessionContext carries the authenticated dashboard subject
// derived from a valid session cookie. Distinct from
// auth.Subject — that's bearer-token-derived; this is cookie-
// derived. A request can carry both; handlers prefer the
// dashboard session for routes like /v1/account/me when both
// are present.
type SessionContext struct {
	Session platform.Session
	User    platform.User
	Account platform.Account
}

// WithSession plants a SessionContext on the context.
func WithSession(ctx context.Context, sc SessionContext) context.Context {
	return context.WithValue(ctx, sessionKey{}, sc)
}

// SessionFromContext extracts the SessionContext if present.
// ok=false when no session was attached (anonymous request).
func SessionFromContext(ctx context.Context) (SessionContext, bool) {
	sc, ok := ctx.Value(sessionKey{}).(SessionContext)
	return sc, ok
}
