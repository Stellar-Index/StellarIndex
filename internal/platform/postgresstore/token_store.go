package postgresstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TokenStore implements [platform.TokenStore] against Postgres.
//
// Magic-link tokens and invites share a 32-byte sha256 token-
// hash key shape but live in distinct tables — the
// authorisation envelope (purpose / role / acceptance) differs
// enough that one-table-many-purposes would force callers to
// remember which fields apply to which row.
type TokenStore struct {
	s   *Store
	now func() time.Time
}

// NewTokenStore returns the Postgres-backed implementation.
// `now` defaults to time.Now.UTC; tests inject a fixed clock.
func NewTokenStore(s *Store) *TokenStore {
	return &TokenStore{s: s, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock overrides the time source. Tests that pin
// "expired five minutes ago" semantics use this.
func (r *TokenStore) WithClock(now func() time.Time) *TokenStore {
	r.now = now
	return r
}

// CreateMagicLinkToken inserts a new row. Caller already
// generated the random plaintext + computed sha256 and is
// responsible for emailing the plaintext to the user.
func (r *TokenStore) CreateMagicLinkToken(ctx context.Context, t platform.MagicLinkToken) error {
	const q = `
		INSERT INTO magic_link_tokens (
			token_hash, email, purpose, expires_at, requested_ip
		)
		VALUES ($1, $2, $3, $4, $5)
	`
	_, err := r.s.db.ExecContext(ctx, q,
		t.TokenHash, t.Email, string(t.Purpose), t.ExpiresAt, ipString(t.RequestedIP),
	)
	if err != nil {
		return fmt.Errorf("create magic link token: %w", err)
	}
	return nil
}

// ConsumeMagicLinkToken atomically marks the row consumed and
// returns it. Three terminal states surface as distinct errors:
//
//   - row absent → platform.ErrNotFound
//   - row already consumed → platform.ErrNotFound (treated the
//     same so a leaked token reused after consumption isn't
//     distinguishable from a typo'd one)
//   - row exists but past expires_at → platform.ErrTokenExpired
//
// The atomic UPDATE...RETURNING combines the lookup, expiry
// check, and consumption-marking so two concurrent requests
// can't both succeed.
func (r *TokenStore) ConsumeMagicLinkToken(ctx context.Context, tokenHash []byte) (platform.MagicLinkToken, error) {
	now := r.now()
	const q = `
		UPDATE magic_link_tokens
		SET consumed_at = $2
		WHERE token_hash = $1
		  AND consumed_at IS NULL
		  AND expires_at > $2
		RETURNING token_hash, email, purpose, expires_at, consumed_at,
		          requested_ip, created_at
	`
	row := r.s.db.QueryRowContext(ctx, q, tokenHash, now)

	var (
		out        platform.MagicLinkToken
		consumedAt sql.NullTime
		ipText     string
	)
	if err := row.Scan(
		&out.TokenHash, &out.Email, &out.Purpose,
		&out.ExpiresAt, &consumedAt, &ipText, &out.CreatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Distinguish expired-but-present from absent: a second
			// query without the expiry guard. Cheap because the
			// happy path is the UPDATE above; this only runs on
			// failure.
			return platform.MagicLinkToken{}, r.classifyMagicLinkMiss(ctx, tokenHash)
		}
		return platform.MagicLinkToken{}, fmt.Errorf("consume magic link token: %w", err)
	}
	if consumedAt.Valid {
		out.ConsumedAt = consumedAt.Time
	}
	return out, nil
}

// ConsumableLoginCandidates returns active login tokens for an email
// whose attempt count is still under maxAttempts. The verify-code
// handler recomputes each row's 6-digit code from its hash and matches
// the user-supplied code, so we return the full rows (TokenHash is the
// load-bearing field).
func (r *TokenStore) ConsumableLoginCandidates(ctx context.Context, email string, maxAttempts int) ([]platform.MagicLinkToken, error) {
	now := r.now()
	const q = `
		SELECT token_hash, email, purpose, expires_at, consumed_at,
		       requested_ip, created_at, attempts
		FROM magic_link_tokens
		WHERE email = $1
		  AND purpose = 'login'
		  AND consumed_at IS NULL
		  AND expires_at > $2
		  AND attempts < $3
		ORDER BY created_at DESC
	`
	rows, err := r.s.db.QueryContext(ctx, q, email, now, maxAttempts)
	if err != nil {
		return nil, fmt.Errorf("consumable login candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []platform.MagicLinkToken
	for rows.Next() {
		var (
			t          platform.MagicLinkToken
			consumedAt sql.NullTime
			ipText     string
		)
		if err := rows.Scan(
			&t.TokenHash, &t.Email, &t.Purpose, &t.ExpiresAt,
			&consumedAt, &ipText, &t.CreatedAt, &t.Attempts,
		); err != nil {
			return nil, fmt.Errorf("consumable login candidates scan: %w", err)
		}
		if consumedAt.Valid {
			t.ConsumedAt = consumedAt.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// IncrementLoginCodeAttempts bumps attempts on every active login
// token for the email. A wrong code thus moves all in-flight tokens
// closer to the cap, after which ConsumableLoginCandidates stops
// returning them.
func (r *TokenStore) IncrementLoginCodeAttempts(ctx context.Context, email string) error {
	now := r.now()
	const q = `
		UPDATE magic_link_tokens
		SET attempts = attempts + 1
		WHERE email = $1
		  AND purpose = 'login'
		  AND consumed_at IS NULL
		  AND expires_at > $2
	`
	if _, err := r.s.db.ExecContext(ctx, q, email, now); err != nil {
		return fmt.Errorf("increment login code attempts: %w", err)
	}
	return nil
}

// RegisterFailedLoginCode records one failed 6-digit-code attempt
// against the EMAIL (migration 0122, C3-032) and returns the resulting
// lockout state.
//
// The whole point is that this counter is independent of any token:
// `magic_link_tokens.attempts` resets to 0 on every new mint, and the
// only thing bounding re-mints is a Redis-backed send throttle that a
// flush, a fail-over or a restart clears. An attacker who re-mints
// therefore had an indefinite ~25-guesses/hour budget against a ~1e6
// code space. Here the budget is per address and survives all of that.
//
// One statement, so concurrent verify attempts for the same address
// cannot both read `failed_count = n` and both write `n+1`:
//
//	insert   — no row yet: first failure of a fresh window.
//	conflict — a row exists. If its window has fully elapsed AND it is
//	           not currently locked, the window restarts at 1;
//	           otherwise the count increments. Crossing maxFailures
//	           stamps locked_until; an already-locked row keeps its
//	           existing lock rather than sliding it forward, so a
//	           grinder cannot extend a victim's lockout indefinitely.
func (r *TokenStore) RegisterFailedLoginCode(
	ctx context.Context, email string, maxFailures int, window, lockFor time.Duration,
) (platform.LoginCodeLockout, error) {
	if email == "" {
		return platform.LoginCodeLockout{}, errors.New("postgresstore: RegisterFailedLoginCode: email is empty")
	}
	if maxFailures <= 0 || window <= 0 || lockFor <= 0 {
		return platform.LoginCodeLockout{}, fmt.Errorf(
			"postgresstore: RegisterFailedLoginCode: bad policy (maxFailures=%d window=%s lockFor=%s)",
			maxFailures, window, lockFor)
	}
	now := r.now()
	const q = `
		INSERT INTO login_code_lockouts
		    (email, failed_count, window_started_at, last_failed_at, created_at, updated_at)
		VALUES ($1, 1, $2, $2, $2, $2)
		ON CONFLICT (email) DO UPDATE SET
		    failed_count = CASE
		        WHEN login_code_lockouts.window_started_at < $2 - make_interval(secs => $3)
		         AND (login_code_lockouts.locked_until IS NULL OR login_code_lockouts.locked_until <= $2)
		        THEN 1
		        ELSE login_code_lockouts.failed_count + 1 END,
		    window_started_at = CASE
		        WHEN login_code_lockouts.window_started_at < $2 - make_interval(secs => $3)
		         AND (login_code_lockouts.locked_until IS NULL OR login_code_lockouts.locked_until <= $2)
		        THEN $2
		        ELSE login_code_lockouts.window_started_at END,
		    last_failed_at = $2,
		    updated_at     = $2
		RETURNING failed_count, window_started_at, locked_until
	`
	var out platform.LoginCodeLockout
	var lockedUntil sql.NullTime
	if err := r.s.db.QueryRowContext(ctx, q,
		email, now, window.Seconds(),
	).Scan(&out.FailedCount, &out.WindowStartedAt, &lockedUntil); err != nil {
		return platform.LoginCodeLockout{}, fmt.Errorf("register failed login code: %w", err)
	}
	if lockedUntil.Valid {
		out.LockedUntil = lockedUntil.Time
	}
	// Arm the lock on the crossing failure. Separate statement rather
	// than a third CASE arm: the threshold comparison needs the
	// POST-increment count, and re-deriving it inside the same UPSERT
	// would duplicate the increment expression a third time.
	if out.FailedCount >= maxFailures && !out.Locked(now) {
		const lockQ = `
			UPDATE login_code_lockouts
			   SET locked_until = $2 + make_interval(secs => $3),
			       updated_at   = $2
			 WHERE email = $1
			   AND (locked_until IS NULL OR locked_until <= $2)
			RETURNING locked_until
		`
		var locked sql.NullTime
		err := r.s.db.QueryRowContext(ctx, lockQ, email, now, lockFor.Seconds()).Scan(&locked)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// A concurrent attempt armed it first; re-read below.
		case err != nil:
			return out, fmt.Errorf("register failed login code: arm lock: %w", err)
		case locked.Valid:
			out.LockedUntil = locked.Time
		}
	}
	return out, nil
}

// LoginCodeLockoutStatus reports the stored lockout for an email
// without recording an attempt.
//
// A missing row reports the zero value. An EXISTING row is returned as
// stored — an elapsed window or an expired lock is NOT normalised away
// here, because the raw counters are what an operator investigating a
// grinder wants to see. Callers decide with
// [platform.LoginCodeLockout.Locked], which is time-aware; the window's
// elapse is applied by [TokenStore.RegisterFailedLoginCode] at the next
// failure.
func (r *TokenStore) LoginCodeLockoutStatus(ctx context.Context, email string) (platform.LoginCodeLockout, error) {
	if email == "" {
		return platform.LoginCodeLockout{}, errors.New("postgresstore: LoginCodeLockoutStatus: email is empty")
	}
	const q = `
		SELECT failed_count, window_started_at, locked_until
		  FROM login_code_lockouts
		 WHERE email = $1
	`
	var out platform.LoginCodeLockout
	var lockedUntil sql.NullTime
	err := r.s.db.QueryRowContext(ctx, q, email).
		Scan(&out.FailedCount, &out.WindowStartedAt, &lockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return platform.LoginCodeLockout{}, nil
	}
	if err != nil {
		return platform.LoginCodeLockout{}, fmt.Errorf("login code lockout status: %w", err)
	}
	if lockedUntil.Valid {
		out.LockedUntil = lockedUntil.Time
	}
	return out, nil
}

// ClearLoginCodeLockout drops the durable counter after a successful
// sign-in through either door. Idempotent — no row is not an error.
func (r *TokenStore) ClearLoginCodeLockout(ctx context.Context, email string) error {
	if email == "" {
		return errors.New("postgresstore: ClearLoginCodeLockout: email is empty")
	}
	if _, err := r.s.db.ExecContext(ctx,
		`DELETE FROM login_code_lockouts WHERE email = $1`, email); err != nil {
		return fmt.Errorf("clear login code lockout: %w", err)
	}
	return nil
}

// SweepLoginCodeLockouts deletes SETTLED lockout rows older than
// `olderThan`, returning how many were removed. Drives
// [logincodereaper.Reaper]; see that package for why this exists.
//
// The short version: `login_code_lockouts.email` is attacker-chosen.
// POST /v1/auth/verify-code is unauthenticated and accepts any
// well-formed address, so one wrong guess against a synthetic address
// inserts a row that ClearLoginCodeLockout will never remove — nobody
// can sign in as an address that does not exist. Bounded only by the
// anonymous rate limit, that is remote, cheap, unbounded table growth.
//
// "Settled" is the load-bearing word: a row with a LIVE lock is never
// deleted, at any age. Reaping the rest costs nothing — retention is
// longer than the counting window, so a sweepable row's window has
// already elapsed and its next failure would have restarted the count
// anyway.
func (r *TokenStore) SweepLoginCodeLockouts(ctx context.Context, olderThan time.Time) (int64, error) {
	const q = `
		DELETE FROM login_code_lockouts
		 WHERE updated_at < $1
		   AND (locked_until IS NULL OR locked_until <= $2)
	`
	res, err := r.s.db.ExecContext(ctx, q, olderThan, r.now())
	if err != nil {
		return 0, fmt.Errorf("sweep login code lockouts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sweep login code lockouts: rows affected: %w", err)
	}
	return n, nil
}

// CountLoginCodeLockouts returns the current row count. Feeds the
// [obs.LoginCodeLockoutRows] gauge so table growth is visible on a
// metric an operator already watches, rather than first appearing as a
// volume-level disk-full page.
func (r *TokenStore) CountLoginCodeLockouts(ctx context.Context) (int64, error) {
	var n int64
	if err := r.s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM login_code_lockouts`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count login code lockouts: %w", err)
	}
	return n, nil
}

// classifyMagicLinkMiss runs after the consume UPDATE found no
// rows. Distinguishes:
//   - row exists, past expiry → ErrTokenExpired
//   - row exists, already consumed → ErrNotFound (intentional;
//     re-use after consumption looks identical to "wrong token")
//   - row absent → ErrNotFound
func (r *TokenStore) classifyMagicLinkMiss(ctx context.Context, tokenHash []byte) error {
	now := r.now()
	const q = `
		SELECT expires_at, consumed_at
		FROM magic_link_tokens
		WHERE token_hash = $1
	`
	var expiresAt time.Time
	var consumedAt sql.NullTime
	err := r.s.db.QueryRowContext(ctx, q, tokenHash).Scan(&expiresAt, &consumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return platform.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("classify magic link miss: %w", err)
	}
	if consumedAt.Valid {
		return platform.ErrNotFound
	}
	if !expiresAt.After(now) {
		return platform.ErrTokenExpired
	}
	// Shouldn't reach here — the row was consumable but the
	// UPDATE missed it. Concurrent consumption from another
	// process. Treat as not-found.
	return platform.ErrNotFound
}

// ─── Invites ─────────────────────────────────────────────────────

const inviteColumns = `
	token_hash, account_id, email, role,
	invited_by_user_id, expires_at,
	accepted_at, revoked_at, created_at
`

func scanInvite(row interface {
	Scan(...any) error
},
) (platform.Invite, error) {
	var i platform.Invite
	var acceptedAt, revokedAt sql.NullTime
	if err := row.Scan(
		&i.TokenHash,
		&i.AccountID,
		&i.Email,
		&i.Role,
		&i.InvitedByUserID,
		&i.ExpiresAt,
		&acceptedAt,
		&revokedAt,
		&i.CreatedAt,
	); err != nil {
		return platform.Invite{}, err
	}
	if acceptedAt.Valid {
		i.AcceptedAt = acceptedAt.Time
	}
	if revokedAt.Valid {
		i.RevokedAt = revokedAt.Time
	}
	return i, nil
}

func (r *TokenStore) CreateInvite(ctx context.Context, i platform.Invite) error {
	const q = `
		INSERT INTO invites (
			token_hash, account_id, email, role,
			invited_by_user_id, expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6)
	`
	_, err := r.s.db.ExecContext(ctx, q,
		i.TokenHash, i.AccountID, i.Email, string(i.Role),
		i.InvitedByUserID, i.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("create invite: %w", err)
	}
	return nil
}

// AcceptInvite atomically marks accepted + returns the row.
// Same expired/not-found classification as ConsumeMagicLinkToken.
func (r *TokenStore) AcceptInvite(ctx context.Context, tokenHash []byte) (platform.Invite, error) {
	now := r.now()
	const q = `
		UPDATE invites
		SET accepted_at = $2
		WHERE token_hash = $1
		  AND accepted_at IS NULL
		  AND revoked_at IS NULL
		  AND expires_at > $2
		RETURNING ` + inviteColumns

	row := r.s.db.QueryRowContext(ctx, q, tokenHash, now)
	out, err := scanInvite(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.Invite{}, r.classifyInviteMiss(ctx, tokenHash)
		}
		return platform.Invite{}, fmt.Errorf("accept invite: %w", err)
	}
	return out, nil
}

func (r *TokenStore) classifyInviteMiss(ctx context.Context, tokenHash []byte) error {
	now := r.now()
	const q = `
		SELECT expires_at, accepted_at, revoked_at
		FROM invites
		WHERE token_hash = $1
	`
	var expiresAt time.Time
	var acceptedAt, revokedAt sql.NullTime
	err := r.s.db.QueryRowContext(ctx, q, tokenHash).Scan(&expiresAt, &acceptedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return platform.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("classify invite miss: %w", err)
	}
	if acceptedAt.Valid || revokedAt.Valid {
		return platform.ErrNotFound
	}
	if !expiresAt.After(now) {
		return platform.ErrTokenExpired
	}
	return platform.ErrNotFound
}

// RevokeInvite marks revoked. Owner / admin uses this to
// cancel pending invites. Idempotent.
func (r *TokenStore) RevokeInvite(ctx context.Context, tokenHash []byte) error {
	const q = `
		UPDATE invites
		SET revoked_at = now()
		WHERE token_hash = $1
		  AND revoked_at IS NULL
		  AND accepted_at IS NULL
	`
	_, err := r.s.db.ExecContext(ctx, q, tokenHash)
	if err != nil {
		return fmt.Errorf("revoke invite: %w", err)
	}
	return nil
}

// ListInvitesForAccount returns active (unrevoked, unaccepted)
// invites — used by the team-management UI.
func (r *TokenStore) ListInvitesForAccount(ctx context.Context, accountID uuid.UUID) ([]platform.Invite, error) {
	// COR-15 (audit-2026-07-23): this used to filter on SQL `now()`
	// (Postgres server time) instead of `r.now()` like every other
	// expiry check in this file — the one method [WithClock]'s
	// injected clock had no effect on, contradicting both the
	// convention every sibling method follows and this type's own
	// "tests use WithClock" doc claim.
	now := r.now()
	const q = `
		SELECT ` + inviteColumns + `
		FROM invites
		WHERE account_id = $1
		  AND accepted_at IS NULL
		  AND revoked_at IS NULL
		  AND expires_at > $2
		ORDER BY created_at DESC
	`
	rows, err := r.s.db.QueryContext(ctx, q, accountID, now)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []platform.Invite
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, fmt.Errorf("list invites scan: %w", err)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// Compile-time interface check.
var _ platform.TokenStore = (*TokenStore)(nil)
