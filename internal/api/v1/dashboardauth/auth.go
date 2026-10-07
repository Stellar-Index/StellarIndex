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

// SessionCookieName is the HTTP cookie name used for dashboard
// sessions. Distinct from any future API-side cookies so a
// browser logged into both can't leak credentials between
// surfaces. The __Host- prefix makes browsers accept it only as a
// host-only, Secure, Path=/ cookie (see [credentialCookie]).
const SessionCookieName = "__Host-stellarindex_session"

// MagicLinkPlaintextLen — the random-bytes length we use for
// magic-link tokens. 32 bytes = 256 bits = preimage-safe;
// the hex-encoded form is what the user sees in the URL.
const MagicLinkPlaintextLen = 32

// SessionTokenLen — the random-bytes length of the session cookie
// token. 32 bytes = 256 bits: the cookie carries this token (hex),
// and the DB stores only sha256(token), so read-access to the
// sessions table is not directly replayable.
const SessionTokenLen = 32

// HashSessionToken returns sha256 of a session cookie token. The
// authentication path hashes the incoming cookie with this and looks
// the session up by hash; mintSession stores the same hash. Mirrors
// [HashMagicLinkPlaintext] — the plaintext is never persisted.
func HashSessionToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

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
// Why keyed: were the code an unkeyed, public function of
// magic_link_tokens.token_hash, anyone with a read of that table (SQL
// injection on any other surface, a stolen backup, a curious operator)
// could compute every in-flight sign-in code DIRECTLY — no brute force,
// no email access — and mint a session for any address they could
// trigger a login for via POST /v1/auth/verify-code. The token
// PLAINTEXT is not recoverable (preimage-safe), but that would not
// matter: the code would be equivalent to the token, and public
// knowledge given the row.
//
// The code is HMAC-SHA256(secret, domain || token_hash) reduced to 6
// digits. The secret lives in config/env (never in Postgres), so a
// database read alone yields nothing: without the secret the code is
// uniformly unpredictable, and the online-guessing bounds (per-token
// attempt cap + durable per-email lockout) are the only attack surface
// left.
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
	mac := hmac.New(sha256.New, mustPurposeKey(secret, loginCodeDomain))
	mac.Write([]byte(loginCodeDomain))
	mac.Write(hash)
	sum := mac.Sum(nil)
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum[:4])%1_000_000)
}

// purposeKey derives the independent HMAC key one consumer of the server
// secret uses (HKDF-SHA256, the consumer's domain string as info), so no
// two purposes — codes, ceremony cookies, login intents, device markers —
// share key material.
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
	// Secret is the root server secret. It is never used as a MAC key
	// itself: each consumer derives its own key with [purposeKey] — the
	// 6-digit code ([Generator.CodeForHash]), the passkey-ceremony cookie
	// (passkeyCeremonyMAC), the magic-link login-intent tag
	// (loginIntentTag) and the login-device marker (loginDeviceMAC).
	// Production wires it from the STELLARINDEX_DASHBOARD_CODE_SECRET env
	// (config api.dashboard.code_secret_env). When empty,
	// Config.validate() refuses to start if Passkeys is wired (a
	// per-process key breaks every ceremony that crosses instances or a
	// restart) and otherwise fills a random per-process secret.
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

// NewSessionToken mints (plaintext token, sha256 hash). The
// plaintext goes in the cookie; the hash is stored on the session
// row. crypto/rand in production; tests inject a fixed source via
// Generator.Read. Mirrors [Generator.NewToken] without the 6-digit
// code (sessions have no paste-friendly variant).
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
