// Package dashboardauth implements the magic-link login flow
// + session middleware for the customer dashboard.
//
// Distinct from internal/auth (bearer-token API auth): that package yields a Subject scoped to a
// single API key; this one handles cookie sessions, scoped to a User (and via User.AccountID to
// an Account). The surfaces do not intersect in v1: dashboard endpoints accept sessions only and
// programmatic endpoints accept keys only.
package dashboardauth

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// SessionCookieName is the dashboard session cookie. The __Host- prefix makes browsers accept it
// only as a host-only, Secure, Path=/ cookie (see [credentialCookie]).
const SessionCookieName = "__Host-stellarindex_session"

// MagicLinkPlaintextLen is the random-byte length of magic-link tokens: 256 bits, preimage-safe.
const MagicLinkPlaintextLen = 32

// SessionTokenLen is the random-byte length of the session token. The DB stores only
// sha256(token), so read access to the sessions table is not replayable.
const SessionTokenLen = 32

// HashSessionToken returns sha256 of a session cookie token; sessions are looked up and stored by
// this hash, never the plaintext (like [HashMagicLinkPlaintext]).
func HashSessionToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// generateMagicLinkToken returns (plaintext, sha256-hash, code): the plaintext goes in the email
// link, the hash is stored in magic_link_tokens, and the paste-friendly 6-digit code is derived
// under secret (see [Generator.CodeForHash]). read is the entropy source; tests inject one.
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

// loginCodeDomain domain-separates the code HMAC from other uses of the server secret.
const loginCodeDomain = "stellarindex/login-code/v1|"

// codeFromHashKeyed derives the 6-digit email code from a stored token hash
// UNDER A SERVER-SIDE SECRET.
//
// Keyed because an unkeyed code would be a public function of magic_link_tokens.token_hash:
// anyone who could read that table (stolen backup, SQL injection, operator) could compute every
// in-flight code and mint a session via POST /v1/auth/verify-code without email access.
//
// The code is HMAC-SHA256(secret, domain || token_hash) reduced to 6 digits. The secret lives in
// config/env, never Postgres, so a database read alone yields nothing and the online-guessing
// bounds (per-token attempt cap, per-email lockout) are the only attack surface. The uint32 %
// 10^6 reduction has negligible modulo bias (0.02%).
func codeFromHashKeyed(secret, hash []byte) string {
	if len(hash) < 4 {
		// A malformed hash must not yield a guessable constant-derived code.
		return ""
	}
	mac := hmac.New(sha256.New, mustPurposeKey(secret, loginCodeDomain))
	mac.Write([]byte(loginCodeDomain))
	mac.Write(hash)
	sum := mac.Sum(nil)
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum[:4])%1_000_000)
}

// purposeKey derives one consumer's independent HMAC key (HKDF-SHA256, its domain string as info)
// so no two purposes (codes, ceremony cookies, login intents, device markers) share key material.
func purposeKey(root []byte, label string) ([]byte, error) {
	return hkdf.Key(sha256.New, root, nil, label, sha256.Size)
}

// mustPurposeKey is [purposeKey] for the MAC helpers; Config.validate()
// derives once at boot, so an error cannot first surface on a request.
func mustPurposeKey(root []byte, label string) []byte {
	k, err := purposeKey(root, label)
	if err != nil {
		panic(fmt.Sprintf("dashboardauth: derive %s key: %v", label, err))
	}
	return k
}

// HashMagicLinkPlaintext returns the sha256 of a user-supplied token, for the TokenStore lookup.
func HashMagicLinkPlaintext(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// Generator wraps the entropy source; tests inject a fixed one for deterministic plaintexts.
type Generator struct {
	Read func([]byte) (int, error)
	// Secret is the root server secret, never used as a MAC key itself: each consumer derives its
	// own key with [purposeKey] (login code, passkey-ceremony cookie, login-intent tag,
	// login-device marker). Production reads it from STELLARINDEX_DASHBOARD_CODE_SECRET. When
	// empty, Config.validate() refuses to start if Passkeys is wired (a per-process key breaks
	// ceremonies that cross instances or a restart) and otherwise fills a random per-process one.
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

// CodeForHash re-derives the 6-digit code for a stored token hash. verify-code compares it in
// constant time against the user's code; the code itself is never stored.
func (g *Generator) CodeForHash(hash []byte) string {
	return codeFromHashKeyed(g.Secret, hash)
}

// NewSessionToken mints (plaintext token, sha256 hash): the plaintext goes in the cookie, the
// hash on the session row. Like [Generator.NewToken] without the 6-digit code.
func (g *Generator) NewSessionToken() (token string, hash []byte, err error) {
	buf := make([]byte, SessionTokenLen)
	n, err := g.Read(buf)
	if err != nil {
		return "", nil, fmt.Errorf("dashboardauth: session token entropy read: %w", err)
	}
	if n != SessionTokenLen {
		return "", nil, fmt.Errorf("dashboardauth: session token short read: got %d want %d", n, SessionTokenLen)
	}
	token = hex.EncodeToString(buf)
	return token, HashSessionToken(token), nil
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

// SessionContext carries the dashboard subject derived from a valid session cookie, distinct from
// the bearer-derived auth.Subject. When a request carries both, handlers prefer the dashboard
// session for routes like /v1/account/me.
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
