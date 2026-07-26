package platform

import (
	"context"
	"net"
	"time"

	"github.com/google/uuid"
)

// TokenPurpose distinguishes the three flows a single-use email
// token can authorise. Stored on the row so a leaked login
// token can't be replayed against an invite-accept route.
type TokenPurpose string

const (
	TokenPurposeLogin        TokenPurpose = "login"
	TokenPurposeEmailVerify  TokenPurpose = "email-verify"
	TokenPurposeInviteAccept TokenPurpose = "invite-accept"
)

// MagicLinkToken is the row backing a magic-link or email-code
// flow. The plaintext is never stored — TokenHash is sha256 of
// the random bytes the server generated. Callers compare by
// hashing the user-supplied token at consumption time.
type MagicLinkToken struct {
	TokenHash   []byte
	Email       string
	Purpose     TokenPurpose
	ExpiresAt   time.Time
	ConsumedAt  time.Time // zero = unconsumed
	RequestedIP net.IP
	CreatedAt   time.Time
	// Attempts counts failed email-code verification tries against
	// this token. The magic LINK ignores it (consumed by full
	// plaintext); the 6-digit CODE path increments it on each wrong
	// guess and stops treating the token as a code candidate past a
	// cap, bounding brute-force of the small code space.
	Attempts int
}

// Invite is a pending team-member invitation. The TokenHash key
// matches the magic_link_tokens table's shape so a single
// consumption path handles both magic-login and invite-accept
// flows.
type Invite struct {
	TokenHash       []byte
	AccountID       uuid.UUID
	Email           string
	Role            Role
	InvitedByUserID uuid.UUID
	ExpiresAt       time.Time
	AcceptedAt      time.Time // zero = pending
	RevokedAt       time.Time // zero = active
	CreatedAt       time.Time
}

// LoginCodeLockout is the durable per-EMAIL failed-verify state for the
// 6-digit email-code sign-in (migration 0122, C3-032). It is what
// [MagicLinkToken.Attempts] is not: independent of any single token, so
// re-minting a link does not hand an attacker a fresh guess budget.
//
// The zero value means "no failures on record" — which is also what a
// fully-elapsed window and an expired lock report.
type LoginCodeLockout struct {
	// FailedCount is the number of failures inside the current window.
	FailedCount int
	// WindowStartedAt is when the current counting window opened.
	WindowStartedAt time.Time
	// LockedUntil, when in the future, means the code path must refuse
	// to match for this address. Zero = not locked.
	LockedUntil time.Time
}

// Locked reports whether the lockout is in force at `now`.
func (l LoginCodeLockout) Locked(now time.Time) bool {
	return !l.LockedUntil.IsZero() && l.LockedUntil.After(now)
}

// TokenStore persists [MagicLinkToken] and [Invite]. The two
// share the token-hash primary key shape (32-byte sha256) but
// live in separate tables because their lifecycle and
// authorisation rules diverge.
type TokenStore interface {
	// CreateMagicLinkToken inserts and returns nothing. Caller
	// already holds the plaintext (to email it).
	CreateMagicLinkToken(ctx context.Context, t MagicLinkToken) error

	// ConsumeMagicLinkToken looks up by hash, atomically marks
	// consumed, and returns the row. ErrNotFound if absent or
	// already consumed; ErrTokenExpired if past expires_at.
	ConsumeMagicLinkToken(ctx context.Context, tokenHash []byte) (MagicLinkToken, error)

	// ConsumableLoginCandidates returns the active (unconsumed,
	// unexpired) login-purpose tokens for an email whose Attempts
	// count is still below maxAttempts. Backs the email-code sign-in
	// path: the caller recomputes each token's 6-digit code from its
	// hash and matches the user-supplied code. Returns an empty slice
	// (not an error) when none qualify. Ordered most-recent first.
	ConsumableLoginCandidates(ctx context.Context, email string, maxAttempts int) ([]MagicLinkToken, error)

	// IncrementLoginCodeAttempts bumps the Attempts counter on every
	// active (unconsumed, unexpired) login-purpose token for an email.
	// Called after a wrong code so a token self-retires from
	// ConsumableLoginCandidates once it crosses the cap.
	IncrementLoginCodeAttempts(ctx context.Context, email string) error

	// RegisterFailedLoginCode records ONE failed code attempt against
	// the email itself — the durable dimension a token re-mint cannot
	// reset (C3-032). `attempts` on magic_link_tokens is per-mint, and
	// the send throttle that bounds re-mints is Redis-only, so without
	// this an attacker re-mints for a fresh 5-guess budget indefinitely.
	//
	// Returns the resulting [LoginCodeLockout]. Once failures inside
	// `window` reach `maxFailures` the email is locked for `lockFor`;
	// callers refuse to match a code while LockedUntil is in the future.
	// A window that has fully elapsed starts fresh.
	RegisterFailedLoginCode(
		ctx context.Context, email string, maxFailures int, window, lockFor time.Duration,
	) (LoginCodeLockout, error)

	// LoginCodeLockoutStatus reports the STORED lockout for an email
	// without recording an attempt. A missing row reports the zero
	// value; an existing row is returned as stored — an elapsed window
	// or an expired lock is not normalised away, because the raw
	// counters are what an operator investigating a grinder reads.
	// Callers decide with [LoginCodeLockout.Locked], which is
	// time-aware.
	LoginCodeLockoutStatus(ctx context.Context, email string) (LoginCodeLockout, error)

	// ClearLoginCodeLockout drops the durable failure counter for an
	// email. Called on a SUCCESSFUL sign-in through either door (code or
	// magic link): possession of a live token proves the address's owner
	// is present, which retires the suspicion the counter represents.
	// Without it a legitimate user's typos would accumulate across
	// months and eventually lock them out of the code path for nothing.
	ClearLoginCodeLockout(ctx context.Context, email string) error

	// CreateInvite inserts.
	CreateInvite(ctx context.Context, i Invite) error

	// AcceptInvite atomically marks accepted + returns the row.
	// Same error semantics as ConsumeMagicLinkToken.
	AcceptInvite(ctx context.Context, tokenHash []byte) (Invite, error)

	// RevokeInvite marks revoked. Owner / admin can call it on
	// pending invites they want to cancel.
	RevokeInvite(ctx context.Context, tokenHash []byte) error

	// ListInvitesForAccount returns active (unrevoked, unaccepted)
	// invites. Used by the team-management UI.
	ListInvitesForAccount(ctx context.Context, accountID uuid.UUID) ([]Invite, error)
}
