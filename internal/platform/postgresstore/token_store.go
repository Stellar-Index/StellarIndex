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

// TokenStore implements [platform.TokenStore] against Postgres; magic-link tokens and invites live in distinct tables because their authorisation envelopes differ.
type TokenStore struct {
	s              *Store
	now            func() time.Time
	sweepBatchRows int
}

// NewTokenStore returns the Postgres-backed implementation; the clock defaults to time.Now.UTC.
func NewTokenStore(s *Store) *TokenStore {
	return &TokenStore{s: s, now: func() time.Time { return time.Now().UTC() }, sweepBatchRows: defaultSweepBatchRows}
}

// WithClock overrides the time source, for tests.
func (r *TokenStore) WithClock(now func() time.Time) *TokenStore {
	r.now = now
	return r
}

// WithSweepBatchSize overrides the per-statement row cap of the reaper sweeps (default defaultSweepBatchRows); tests use a small value.
func (r *TokenStore) WithSweepBatchSize(n int) *TokenStore {
	r.sweepBatchRows = n
	return r
}

// defaultSweepBatchRows and sweepMaxBatchesPerCall bound every reaper DELETE: these tables grow at a rate an unauthenticated caller controls, and one unbounded DELETE would hold locks and WAL on the shared pool.
// Each sweep loops bounded `DELETE ... WHERE pk IN (SELECT ... LIMIT)` batches; the remainder waits for the next call.
const (
	defaultSweepBatchRows  = 2000
	sweepMaxBatchesPerCall = 25
)

// CreateMagicLinkToken inserts a row; the caller generates the random plaintext and sha256 and emails the plaintext.
func (r *TokenStore) CreateMagicLinkToken(ctx context.Context, t platform.MagicLinkToken) error {
	ip, err := ipString(t.RequestedIP)
	if err != nil {
		return fmt.Errorf("create magic link token: %w", err)
	}
	const q = `
		INSERT INTO magic_link_tokens (
			token_hash, email, purpose, expires_at, requested_ip
		)
		VALUES ($1, $2, $3, $4, $5)
	`
	if _, err := r.s.db.ExecContext(ctx, q,
		t.TokenHash, t.Email, string(t.Purpose), t.ExpiresAt, ip,
	); err != nil {
		return fmt.Errorf("create magic link token: %w", err)
	}
	return nil
}

// ConsumeMagicLinkToken atomically marks the row consumed in one UPDATE...RETURNING; absent or already-consumed gives platform.ErrNotFound, past expires_at gives platform.ErrTokenExpired.
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
			// Distinguish expired-but-present from absent with a second query lacking the expiry guard; only runs on failure, the happy path is
			// the UPDATE above.
			return platform.MagicLinkToken{}, r.classifyMagicLinkMiss(ctx, tokenHash)
		}
		return platform.MagicLinkToken{}, fmt.Errorf("consume magic link token: %w", err)
	}
	if consumedAt.Valid {
		out.ConsumedAt = consumedAt.Time
	}
	return out, nil
}

// ReserveLoginCode charges one attempt to the email's NEWEST active login token if under maxAttempts, atomically (FOR UPDATE plus re-check of `attempts < $3`), and returns the row; older tokens never widen the guessable set.
func (r *TokenStore) ReserveLoginCode(ctx context.Context, email string, maxAttempts int) (platform.MagicLinkToken, error) {
	now := r.now()
	const q = `
		WITH newest AS (
		    SELECT token_hash
		      FROM magic_link_tokens
		     WHERE email = $1
		       AND purpose = 'login'
		       AND consumed_at IS NULL
		       AND expires_at > $2
		     ORDER BY created_at DESC, token_hash DESC
		     LIMIT 1
		       FOR UPDATE
		)
		UPDATE magic_link_tokens m
		   SET attempts = m.attempts + 1
		  FROM newest
		 WHERE m.token_hash = newest.token_hash
		   AND m.attempts < $3
		RETURNING m.token_hash, m.email, m.purpose, m.expires_at, m.consumed_at,
		          m.requested_ip, m.created_at, m.attempts
	`
	var (
		t          platform.MagicLinkToken
		consumedAt sql.NullTime
		ipText     string
	)
	err := r.s.db.QueryRowContext(ctx, q, email, now, maxAttempts).Scan(
		&t.TokenHash, &t.Email, &t.Purpose, &t.ExpiresAt,
		&consumedAt, &ipText, &t.CreatedAt, &t.Attempts,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return platform.MagicLinkToken{}, platform.ErrNotFound
	}
	if err != nil {
		return platform.MagicLinkToken{}, fmt.Errorf("reserve login code: %w", err)
	}
	if consumedAt.Valid {
		t.ConsumedAt = consumedAt.Time
	}
	return t, nil
}

// HasLiveLoginCode is ReserveLoginCode's selection without the charge: true only when the newest token is still under maxAttempts.
func (r *TokenStore) HasLiveLoginCode(ctx context.Context, email string, maxAttempts int) (bool, error) {
	const q = `
		SELECT COALESCE((
		    SELECT attempts < $3
		      FROM magic_link_tokens
		     WHERE email = $1
		       AND purpose = 'login'
		       AND consumed_at IS NULL
		       AND expires_at > $2
		     ORDER BY created_at DESC, token_hash DESC
		     LIMIT 1
		), false)
	`
	var live bool
	if err := r.s.db.QueryRowContext(ctx, q, email, r.now(), maxAttempts).Scan(&live); err != nil {
		return false, fmt.Errorf("has live login code: %w", err)
	}
	return live, nil
}

// RegisterFailedLoginCode records one failed code attempt against the EMAIL (independent of any token, so re-minting cannot reset the guess budget) in one statement, stamping locked_until at maxFailures and never extending an existing lock.
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
	// Arm the lock on the crossing failure in a separate statement: the threshold comparison needs the POST-increment count, and a third
	// CASE arm would duplicate the increment expression.
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

// LoginCodeLockoutStatus reports the stored lockout without recording an attempt; a missing row is the zero value and an existing row is returned as stored (elapsed windows are not normalised), so callers decide with the time-aware [platform.LoginCodeLockout.Locked].
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

// ClearLoginCodeLockout drops the durable counter after a successful sign-in; idempotent.
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

// SweepLoginCodeLockouts deletes SETTLED lockout rows older than olderThan (email is attacker-chosen, so rows would grow unbounded), never a LIVE lock; the bool reports the per-call cap was hit so the reaper re-sweeps.
func (r *TokenStore) SweepLoginCodeLockouts(ctx context.Context, olderThan time.Time) (int64, bool, error) {
	const q = `
		DELETE FROM login_code_lockouts
		 WHERE email IN (
		     SELECT email FROM login_code_lockouts
		      WHERE updated_at < $1
		        AND (locked_until IS NULL OR locked_until <= $2)
		      LIMIT $3
		 )
	`
	var deleted int64
	for i := 0; i < sweepMaxBatchesPerCall; i++ {
		res, err := r.s.db.ExecContext(ctx, q, olderThan, r.now(), r.sweepBatchRows)
		if err != nil {
			return deleted, false, fmt.Errorf("sweep login code lockouts: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, false, fmt.Errorf("sweep login code lockouts: rows affected: %w", err)
		}
		deleted += n
		if n < int64(r.sweepBatchRows) {
			return deleted, false, nil
		}
	}
	return deleted, true, nil
}

// CountLoginCodeLockouts returns the row count, feeding the [obs.LoginCodeLockoutRows] gauge.
func (r *TokenStore) CountLoginCodeLockouts(ctx context.Context) (int64, error) {
	var n int64
	if err := r.s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM login_code_lockouts`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count login code lockouts: %w", err)
	}
	return n, nil
}

// SweepExpiredMagicLinkTokens deletes rows with expires_at before olderThan (email and requested_ip are attacker-chosen PII, so unbounded otherwise); retained a while so classifyMagicLinkMiss can tell expired from absent.
func (r *TokenStore) SweepExpiredMagicLinkTokens(ctx context.Context, olderThan time.Time) (int64, error) {
	const q = `
		DELETE FROM magic_link_tokens
		 WHERE token_hash IN (
		     SELECT token_hash FROM magic_link_tokens
		      WHERE expires_at < $1
		      LIMIT $2
		 )
	`
	var total int64
	for i := 0; i < sweepMaxBatchesPerCall; i++ {
		res, err := r.s.db.ExecContext(ctx, q, olderThan, r.sweepBatchRows)
		if err != nil {
			return total, fmt.Errorf("sweep expired magic link tokens: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("sweep expired magic link tokens: rows affected: %w", err)
		}
		total += n
		if n < int64(r.sweepBatchRows) {
			break
		}
	}
	return total, nil
}

// CountMagicLinkTokens returns the row count, feeding the [obs.MagicLinkTokenRows] gauge.
func (r *TokenStore) CountMagicLinkTokens(ctx context.Context) (int64, error) {
	var n int64
	if err := r.s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM magic_link_tokens`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count magic link tokens: %w", err)
	}
	return n, nil
}

// classifyMagicLinkMiss runs after a consume UPDATE found no rows: past expiry gives ErrTokenExpired; consumed or absent gives ErrNotFound (re-use must look like a wrong token).
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

// AcceptInvite atomically marks the invite accepted and returns the row, with the same expired/not-found classification as ConsumeMagicLinkToken.
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

// RevokeInvite marks an invite revoked so an owner or admin can cancel it; idempotent.
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

// ListInvitesForAccount returns active (unrevoked, unaccepted) invites for the team-management UI.
func (r *TokenStore) ListInvitesForAccount(ctx context.Context, accountID uuid.UUID) ([]platform.Invite, error) {
	// Filter on `r.now()`, not SQL `now()` (Postgres server time), like
	// every other expiry check in this file, so [WithClock]'s injected
	// clock governs this method too.
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
