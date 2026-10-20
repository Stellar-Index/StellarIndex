package platform

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Role is the per-account permission level for a user.
type Role string

const (
	RoleOwner   Role = "owner"   // can do everything; required: at least one per account
	RoleAdmin   Role = "admin"   // can do everything except billing + transfer ownership
	RoleBilling Role = "billing" // billing-only; can't mint keys or invite
	RoleMember  Role = "member"  // can mint own keys; can't invite or change billing
	RoleViewer  Role = "viewer"  // read-only
)

// placeholderEmailDomain is an RFC 2606 reserved TLD: mail to it can
// never be delivered, so a placeholder address cannot reach a stranger.
const placeholderEmailDomain = "@passkey.invalid"

// PlaceholderEmail is the unique, undeliverable address stored for a passkey user who gave no email (users.email is NOT NULL, unique).
func PlaceholderEmail(userID uuid.UUID) string {
	return "passkey-" + userID.String() + placeholderEmailDomain
}

// IsPlaceholderEmail reports whether email came from [PlaceholderEmail].
func IsPlaceholderEmail(email string) bool {
	return strings.HasSuffix(strings.ToLower(email), placeholderEmailDomain)
}

// User is a human with a dashboard login, attached to exactly one account in v1; IsStaff is orthogonal to Role and gates the admin surface.
type User struct {
	ID                     uuid.UUID
	AccountID              uuid.UUID
	Email                  string
	DisplayName            string
	Role                   Role
	EmailVerifiedAt        time.Time // zero = unverified
	LastLoginAt            time.Time
	MFAEnabled             bool
	MFASecretEnc           []byte // libsodium-sealed; never written to logs
	MFARecoveryCodesHashed [][]byte
	IsStaff                bool
	CreatedAt              time.Time
}

// Session is an authenticated dashboard session backed by an HttpOnly cookie. The cookie carries a random token, NOT the ID; only TokenHash (sha256) is persisted, so a read of the table is not replayable, and ID never leaves the server.
type Session struct {
	ID uuid.UUID
	// TokenHash is sha256(cookie token), the lookup key on the auth path (GetSessionByTokenHash). The plaintext is never stored, like the
	// magic-link plaintext.
	TokenHash    []byte
	UserID       uuid.UUID
	ExpiresAt    time.Time
	RevokedAt    time.Time // zero = active
	CreatedAt    time.Time
	LastSeenAt   time.Time
	IPFirstSeen  net.IP
	IPLastSeen   net.IP
	UserAgent    string
	GeoFirstSeen string // ISO 3166-1 alpha-2; empty if unknown
	GeoLastSeen  string
}

// UserStore persists [User] and [Session].
type UserStore interface {
	// CreateUser inserts. Returns the row with ID + CreatedAt set.
	CreateUser(ctx context.Context, u User) (User, error)

	// GetUserByID returns the user; ErrNotFound if absent.
	GetUserByID(ctx context.Context, id uuid.UUID) (User, error)

	// GetUserByEmail returns the user; ErrNotFound if absent.
	// Email match is case-insensitive (citext column).
	GetUserByEmail(ctx context.Context, email string) (User, error)

	// ListUsersForAccount returns every member of the account.
	// Excludes nothing — caller filters by role / staff if needed.
	ListUsersForAccount(ctx context.Context, accountID uuid.UUID) ([]User, error)

	// UpdateUser writes the mutable fields (display_name, role, email_verified_at, last_login_at, MFA); immutable fields (id, account_id, email, created_at, is_staff) are ignored.
	UpdateUser(ctx context.Context, u User) error

	// CreateSession mints a new dashboard session. The caller sets
	// TokenHash (sha256 of the random cookie token it holds); the row
	// stores only that hash.
	CreateSession(ctx context.Context, s Session) (Session, error)

	// GetSession looks up by the internal PK ID; ErrNotFound if absent
	// or revoked. This is NOT the authentication path — the cookie never
	// carries the ID. It backs internal-handle reads (tests, ops) only.
	GetSession(ctx context.Context, id uuid.UUID) (Session, error)

	// GetSessionByTokenHash looks up an active session by sha256 of the cookie token (ErrNotFound if absent, revoked or expired); the table stores only hashes, so a leak is not replayable.
	GetSessionByTokenHash(ctx context.Context, tokenHash []byte) (Session, error)

	// TouchSession updates LastSeenAt + IPLastSeen + UserAgent.
	// Debounced caller-side to once-per-minute to avoid hot-row
	// contention.
	TouchSession(ctx context.Context, id uuid.UUID, ip net.IP, userAgent string) error

	// RevokeSession marks the session revoked. Idempotent.
	RevokeSession(ctx context.Context, id uuid.UUID) error

	// RevokeAllUserSessions logs out every active session for the
	// user — invoked on password reset, MFA reset, account
	// suspension. Idempotent.
	RevokeAllUserSessions(ctx context.Context, userID uuid.UUID) error

	// RevokeOtherUserSessions revokes every active session for the user
	// except keepSessionID. Idempotent.
	RevokeOtherUserSessions(ctx context.Context, userID, keepSessionID uuid.UUID) error

	// CapUserSessions keeps keepSessionID plus the max-1 most recently
	// created other live sessions of the user and revokes the rest.
	CapUserSessions(ctx context.Context, userID, keepSessionID uuid.UUID, maxLive int) error
}
