package platform

import (
	"context"
	"net"
	"time"

	"github.com/google/uuid"
)

// TokenPurpose names the flow a single-use email token authorises, stored on the row so a leaked login token cannot be replayed against invite-accept.
type TokenPurpose string

const (
	TokenPurposeLogin        TokenPurpose = "login"
	TokenPurposeEmailVerify  TokenPurpose = "email-verify"
	TokenPurposeInviteAccept TokenPurpose = "invite-accept"
)

// MagicLinkToken is the row backing a magic-link or email-code flow; the plaintext is never stored, TokenHash is sha256 compared by hashing the submitted token.
type MagicLinkToken struct {
	TokenHash   []byte
	Email       string
	Purpose     TokenPurpose
	ExpiresAt   time.Time
	ConsumedAt  time.Time // zero = unconsumed
	RequestedIP net.IP
	CreatedAt   time.Time
	// Attempts counts failed email-code tries against this token; the magic LINK ignores it, while the code path increments it per wrong guess and stops treating the token as a candidate past a cap.
	Attempts int
}

// Invite is a pending team-member invitation whose TokenHash key matches magic_link_tokens so one consumption path serves login and invite-accept.
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

// LoginCodeDeviceKey is the lockout key for code attempts from a browser holding the address's login-device proof; it contains a space so it never collides with a request-body email.
func LoginCodeDeviceKey(email string) string { return email + " device" }

// LoginCodeLockout is the durable per-EMAIL failed-verify state for code sign-in, independent of any token so re-minting a link grants no fresh guess budget; the zero value means no failures on record.
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

// TokenStore persists [MagicLinkToken] and [Invite] (same 32-byte sha256 key shape) in separate tables because their lifecycle and authorisation rules diverge.
type TokenStore interface {
	// CreateMagicLinkToken inserts; the caller already holds the plaintext to email.
	CreateMagicLinkToken(ctx context.Context, t MagicLinkToken) error

	// ConsumeMagicLinkToken atomically marks the hash consumed and returns the row. ErrNotFound if
	// absent or already consumed; ErrTokenExpired if past expires_at.
	ConsumeMagicLinkToken(ctx context.Context, tokenHash []byte) (MagicLinkToken, error)

	// ReserveLoginCode charges one attempt to the email's NEWEST active login token if Attempts < maxAttempts and returns it post-increment; compare the submitted code only against that row, so one guess means one code (1 in 1e6).
	// Charge and cap check are atomic; ErrNotFound if no active token or it is at the cap.
	ReserveLoginCode(ctx context.Context, email string, maxAttempts int) (MagicLinkToken, error)

	// HasLiveLoginCode reports, without charging, whether ReserveLoginCode would hand out a candidate, so a request with nothing to compare spends no durable failure budget.
	HasLiveLoginCode(ctx context.Context, email string, maxAttempts int) (bool, error)

	// RegisterFailedLoginCode records ONE failed attempt against the email itself, the durable dimension a re-mint cannot reset (per-mint attempts and the Redis-only send throttle are not enough).
	// Returns the [LoginCodeLockout]: failures in `window` reaching `maxFailures` lock the email for `lockFor`; a fully elapsed window starts fresh.
	RegisterFailedLoginCode(
		ctx context.Context, email string, maxFailures int, window, lockFor time.Duration,
	) (LoginCodeLockout, error)

	// LoginCodeLockoutStatus reports the STORED lockout without recording an attempt (missing row is the zero value, elapsed state is not normalised); callers decide with the time-aware [LoginCodeLockout.Locked].
	LoginCodeLockoutStatus(ctx context.Context, email string) (LoginCodeLockout, error)

	// ClearLoginCodeLockout drops the failure counter on a SUCCESSFUL sign-in through either door, so a legitimate user's typos do not accumulate into a lockout.
	ClearLoginCodeLockout(ctx context.Context, email string) error

	// CreateInvite inserts.
	CreateInvite(ctx context.Context, i Invite) error

	// AcceptInvite atomically marks accepted + returns the row.
	// Same error semantics as ConsumeMagicLinkToken.
	AcceptInvite(ctx context.Context, tokenHash []byte) (Invite, error)

	// RevokeInvite marks a pending invite revoked.
	RevokeInvite(ctx context.Context, tokenHash []byte) error

	// ListInvitesForAccount returns active (unrevoked, unaccepted) invites.
	ListInvitesForAccount(ctx context.Context, accountID uuid.UUID) ([]Invite, error)
}
