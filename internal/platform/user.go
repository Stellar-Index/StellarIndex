package platform

import (
	"context"
	"net"
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

// User is a human with a dashboard login. v1 attaches each user
// to exactly one account; multi-org migrates via a memberships
// table (see Account doc).
//
// IsStaff is orthogonal to Role — a user can be RoleMember on
// their account AND have IsStaff = true (gates the
// admin.stellarindex.io surface).
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

// Session is an authenticated dashboard session backed by an
// HttpOnly cookie. Distinct from API keys: cookie sessions
// authenticate the dashboard SPA; bearer tokens authenticate
// programmatic API calls.
//
// The cookie carries a high-entropy random token, NOT the ID.
// Only TokenHash (sha256 of that token) is persisted, mirroring
// api_keys/magic_link_tokens: a read of the sessions table is
// therefore not directly replayable (W1-auth-passkey-2). ID stays
// the internal primary-key handle used by Touch/Revoke and never
// leaves the server.
type Session struct {
	ID uuid.UUID
	// TokenHash is sha256(cookie token). It is the lookup key on the
	// authentication path (GetSessionByTokenHash). The plaintext token
	// is never stored — the caller holds it just long enough to set the
	// cookie, exactly like the magic-link plaintext.
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

	// UpdateUser writes mutable fields (display_name, role,
	// email_verified_at, last_login_at, MFA fields). Immutable
	// fields (id, account_id, email, created_at, is_staff) are
	// ignored — staff-only operations on those go through dedicated
	// admin methods.
	UpdateUser(ctx context.Context, u User) error

	// CreateSession mints a new dashboard session. The caller sets
	// TokenHash (sha256 of the random cookie token it holds); the row
	// stores only that hash.
	CreateSession(ctx context.Context, s Session) (Session, error)

	// GetSession looks up by the internal PK ID; ErrNotFound if absent
	// or revoked. This is NOT the authentication path — the cookie never
	// carries the ID. It backs internal-handle reads (tests, ops) only.
	GetSession(ctx context.Context, id uuid.UUID) (Session, error)

	// GetSessionByTokenHash looks up an active session by sha256 of the
	// cookie token; ErrNotFound if absent or revoked. This is the
	// authentication path: resolveSession hashes the incoming cookie and
	// looks up by hash, so a leak of the sessions table (which stores
	// only the hash) is not directly replayable.
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
}
