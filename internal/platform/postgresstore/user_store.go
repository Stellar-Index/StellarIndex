package postgresstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Stellar-Index/StellarIndex/internal/pgarray"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// UserStore implements [platform.UserStore] against Postgres.
type UserStore struct {
	s              *Store
	sweepBatchRows int
}

// NewUserStore returns the Postgres-backed implementation.
func NewUserStore(s *Store) *UserStore {
	return &UserStore{s: s, sweepBatchRows: defaultSweepBatchRows}
}

// WithSweepBatchSize overrides the per-statement row cap of SweepEndedSessions, for tests.
func (r *UserStore) WithSweepBatchSize(n int) *UserStore {
	r.sweepBatchRows = n
	return r
}

const userColumns = `
	id, account_id, email,
	COALESCE(display_name, ''),
	role,
	email_verified_at, last_login_at,
	mfa_enabled, mfa_secret_enc, mfa_recovery_codes_hashed,
	is_staff, created_at
`

func scanUser(row interface {
	Scan(...any) error
},
) (platform.User, error) {
	var u platform.User
	var emailVerifiedAt, lastLoginAt sql.NullTime
	if err := row.Scan(
		&u.ID,
		&u.AccountID,
		&u.Email,
		&u.DisplayName,
		&u.Role,
		&emailVerifiedAt,
		&lastLoginAt,
		&u.MFAEnabled,
		&u.MFASecretEnc,
		pgarray.Bytea(&u.MFARecoveryCodesHashed),
		&u.IsStaff,
		&u.CreatedAt,
	); err != nil {
		return platform.User{}, err
	}
	if emailVerifiedAt.Valid {
		u.EmailVerifiedAt = emailVerifiedAt.Time
	}
	if lastLoginAt.Valid {
		u.LastLoginAt = lastLoginAt.Time
	}
	return u, nil
}

// CreateUser inserts a user row; a non-nil u.ID is kept (passkey signup binds it first), nil takes the default.
func (r *UserStore) CreateUser(ctx context.Context, u platform.User) (platform.User, error) {
	const q = `
		INSERT INTO users (
			id, account_id, email, display_name, role,
			mfa_enabled, mfa_secret_enc, mfa_recovery_codes_hashed,
			is_staff
		)
		VALUES (COALESCE($9::uuid, uuid_generate_v4()), $1, $2, NULLIF($3, ''), $4, $5, $6, $7, $8)
		RETURNING ` + userColumns

	var id any
	if u.ID != uuid.Nil {
		id = u.ID
	}
	row := r.s.db.QueryRowContext(ctx, q,
		u.AccountID, u.Email, u.DisplayName, string(u.Role),
		u.MFAEnabled, u.MFASecretEnc,
		u.MFARecoveryCodesHashed,
		u.IsStaff, id,
	)
	out, err := scanUser(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrUniqueViolation {
			return platform.User{}, fmt.Errorf("create user: %w", platform.ErrConflict)
		}
		return platform.User{}, fmt.Errorf("create user: %w", err)
	}
	return out, nil
}

func (r *UserStore) GetUserByID(ctx context.Context, id uuid.UUID) (platform.User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE id = $1`
	row := r.s.db.QueryRowContext(ctx, q, id)
	out, err := scanUser(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.User{}, platform.ErrNotFound
		}
		return platform.User{}, fmt.Errorf("get user by id: %w", err)
	}
	return out, nil
}

func (r *UserStore) GetUserByEmail(ctx context.Context, email string) (platform.User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE email = $1`
	row := r.s.db.QueryRowContext(ctx, q, email)
	out, err := scanUser(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.User{}, platform.ErrNotFound
		}
		return platform.User{}, fmt.Errorf("get user by email: %w", err)
	}
	return out, nil
}

func (r *UserStore) ListUsersForAccount(ctx context.Context, accountID uuid.UUID) ([]platform.User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE account_id = $1 ORDER BY created_at ASC`
	rows, err := r.s.db.QueryContext(ctx, q, accountID)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []platform.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("list users scan: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUser writes the mutable fields; id, account_id, email, created_at and is_staff are immutable and ignored.
func (r *UserStore) UpdateUser(ctx context.Context, u platform.User) error {
	const q = `
		UPDATE users SET
			display_name = NULLIF($2, ''),
			role = $3,
			email_verified_at = $4,
			last_login_at = $5,
			mfa_enabled = $6,
			mfa_secret_enc = $7,
			mfa_recovery_codes_hashed = $8
		WHERE id = $1
	`
	var emailVerifiedAt, lastLoginAt sql.NullTime
	if !u.EmailVerifiedAt.IsZero() {
		emailVerifiedAt = sql.NullTime{Time: u.EmailVerifiedAt, Valid: true}
	}
	if !u.LastLoginAt.IsZero() {
		lastLoginAt = sql.NullTime{Time: u.LastLoginAt, Valid: true}
	}
	res, err := r.s.db.ExecContext(ctx, q,
		u.ID, u.DisplayName, string(u.Role),
		emailVerifiedAt, lastLoginAt,
		u.MFAEnabled, u.MFASecretEnc,
		u.MFARecoveryCodesHashed,
	)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return platform.ErrNotFound
	}
	return nil
}

// ─── Sessions ─────────────────────────────────────────────────────

const sessionColumns = `
	id, token_hash, user_id, expires_at, revoked_at, created_at, last_seen_at,
	ip_first_seen, ip_last_seen, user_agent,
	COALESCE(geo_first_seen, ''), COALESCE(geo_last_seen, '')
`

func scanSession(row interface {
	Scan(...any) error
},
) (platform.Session, error) {
	var s platform.Session
	var revokedAt sql.NullTime
	var ipFirst, ipLast string
	if err := row.Scan(
		&s.ID,
		&s.TokenHash,
		&s.UserID,
		&s.ExpiresAt,
		&revokedAt,
		&s.CreatedAt,
		&s.LastSeenAt,
		&ipFirst,
		&ipLast,
		&s.UserAgent,
		&s.GeoFirstSeen,
		&s.GeoLastSeen,
	); err != nil {
		return platform.Session{}, err
	}
	if revokedAt.Valid {
		s.RevokedAt = revokedAt.Time
	}
	s.IPFirstSeen = net.ParseIP(ipFirst)
	s.IPLastSeen = net.ParseIP(ipLast)
	return s, nil
}

func (r *UserStore) CreateSession(ctx context.Context, s platform.Session) (platform.Session, error) {
	ipFirst, err := ipString(s.IPFirstSeen)
	if err != nil {
		return platform.Session{}, fmt.Errorf("create session: %w", err)
	}
	ipLast, err := ipString(s.IPLastSeen)
	if err != nil {
		return platform.Session{}, fmt.Errorf("create session: %w", err)
	}
	const q = `
		INSERT INTO sessions (
			token_hash, user_id, expires_at,
			ip_first_seen, ip_last_seen, user_agent,
			geo_first_seen, geo_last_seen
		)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''))
		RETURNING ` + sessionColumns

	row := r.s.db.QueryRowContext(ctx, q,
		s.TokenHash, s.UserID, s.ExpiresAt,
		ipFirst, ipLast, s.UserAgent,
		s.GeoFirstSeen, s.GeoLastSeen,
	)
	out, err := scanSession(row)
	if err != nil {
		return platform.Session{}, fmt.Errorf("create session: %w", err)
	}
	return out, nil
}

func (r *UserStore) GetSession(ctx context.Context, id uuid.UUID) (platform.Session, error) {
	const q = `SELECT ` + sessionColumns + ` FROM sessions WHERE id = $1 AND revoked_at IS NULL`
	row := r.s.db.QueryRowContext(ctx, q, id)
	out, err := scanSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.Session{}, platform.ErrNotFound
		}
		return platform.Session{}, fmt.Errorf("get session: %w", err)
	}
	return out, nil
}

// GetSessionByTokenHash is the authentication lookup by cookie-token hash (the table stores only hashes); expired rows are filtered here, since they persist until the reaper's grace window passes.
func (r *UserStore) GetSessionByTokenHash(ctx context.Context, tokenHash []byte) (platform.Session, error) {
	const q = `SELECT ` + sessionColumns + ` FROM sessions
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()`
	row := r.s.db.QueryRowContext(ctx, q, tokenHash)
	out, err := scanSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.Session{}, platform.ErrNotFound
		}
		return platform.Session{}, fmt.Errorf("get session by token hash: %w", err)
	}
	return out, nil
}

// TouchSession updates the last-seen fields, always writing (the caller debounces); a nil ip keeps the existing value via COALESCE rather than a fake 0.0.0.0.
func (r *UserStore) TouchSession(ctx context.Context, id uuid.UUID, ip net.IP, userAgent string) error {
	const q = `
		UPDATE sessions SET
			last_seen_at = now(),
			ip_last_seen = COALESCE(NULLIF($2, '')::inet, ip_last_seen),
			user_agent = $3
		WHERE id = $1 AND revoked_at IS NULL
	`
	// Deliberately NOT ipString(ip), which fails closed on nil for inserts
	// that have no value to fall back to. Here the row exists, so an
	// unresolved ip means "keep ip_last_seen": empty string → NULLIF → COALESCE.
	ipParam := ""
	if ip != nil {
		ipParam = ip.String()
	}
	res, err := r.s.db.ExecContext(ctx, q, id, ipParam, userAgent)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return platform.ErrNotFound
	}
	return nil
}

// RevokeSession sets revoked_at. Idempotent: re-revoking is a no-op.
func (r *UserStore) RevokeSession(ctx context.Context, id uuid.UUID) error {
	const q = `UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`
	_, err := r.s.db.ExecContext(ctx, q, id)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// RevokeAllUserSessions logs the user out everywhere. Idempotent.
func (r *UserStore) RevokeAllUserSessions(ctx context.Context, userID uuid.UUID) error {
	const q = `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`
	_, err := r.s.db.ExecContext(ctx, q, userID)
	if err != nil {
		return fmt.Errorf("revoke all user sessions: %w", err)
	}
	return nil
}

// RevokeOtherUserSessions logs the user out everywhere except keepSessionID; idempotent.
func (r *UserStore) RevokeOtherUserSessions(ctx context.Context, userID, keepSessionID uuid.UUID) error {
	const q = `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL`
	_, err := r.s.db.ExecContext(ctx, q, userID, keepSessionID)
	if err != nil {
		return fmt.Errorf("revoke other user sessions: %w", err)
	}
	return nil
}

// CapUserSessions revokes the user's oldest live sessions beyond maxLive, never keepSessionID; idempotent.
func (r *UserStore) CapUserSessions(ctx context.Context, userID, keepSessionID uuid.UUID, maxLive int) error {
	if maxLive < 1 {
		return fmt.Errorf("cap user sessions: maxLive %d < 1", maxLive)
	}
	const q = `
		UPDATE sessions SET revoked_at = now()
		WHERE id IN (
			SELECT id FROM sessions
			WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL AND expires_at > now()
			ORDER BY created_at DESC, id DESC
			OFFSET $3
		)
	`
	if _, err := r.s.db.ExecContext(ctx, q, userID, keepSessionID, maxLive-1); err != nil {
		return fmt.Errorf("cap user sessions: %w", err)
	}
	return nil
}

// errNilClientIP is returned by ipString when ip is nil.
var errNilClientIP = errors.New("postgresstore: nil client IP")

// ipString renders ip for the `inet NOT NULL` columns. A nil IP is not test-only (clientIP returns nil when RemoteAddr fails to split) and the schema cannot hold NULL, so this fails closed with an error rather than writing a plausible-looking placeholder into a forensics column.
func ipString(ip net.IP) (string, error) {
	if ip == nil {
		return "", errNilClientIP
	}
	return ip.String(), nil
}

// SweepEndedSessions deletes sessions that expired or were revoked before olderThan (driving internal/retentionreaper); both states are terminal since nothing moves expires_at forward.
func (r *UserStore) SweepEndedSessions(ctx context.Context, olderThan time.Time) (int64, error) {
	const q = `
		DELETE FROM sessions
		 WHERE id IN (
		     SELECT id FROM sessions
		      WHERE expires_at < $1 OR revoked_at < $1
		      LIMIT $2
		 )
	`
	return r.s.deleteInBatches(ctx, "sweep ended sessions", q, olderThan, r.sweepBatchRows)
}

// Compile-time interface check.
var _ platform.UserStore = (*UserStore)(nil)
