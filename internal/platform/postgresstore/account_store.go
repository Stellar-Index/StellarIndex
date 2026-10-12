package postgresstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// AccountStore implements [platform.AccountStore] against Postgres, sharing the pool of the [*Store] it is built from.
type AccountStore struct {
	s              *Store
	sweepBatchRows int
}

// NewAccountStore returns the Postgres-backed implementation.
func NewAccountStore(s *Store) *AccountStore {
	return &AccountStore{s: s, sweepBatchRows: defaultSweepBatchRows}
}

// WithSweepBatchSize overrides the per-statement row cap [AccountStore.ReapSuspendedOrphans] uses; see [TokenStore.WithSweepBatchSize].
func (r *AccountStore) WithSweepBatchSize(n int) *AccountStore {
	r.sweepBatchRows = n
	return r
}

const (
	pgErrUniqueViolation = "23505"
)

// accountColumns lists the SELECT projection used by every
// reader; centralised so it stays consistent with the
// scanAccount helper below.
const accountColumns = `
	id, name, slug, billing_email,
	tier, status, created_at,
	suspended_at, COALESCE(suspended_reason, ''),
	COALESCE(rate_limit_per_min_override, 0),
	COALESCE(monthly_request_quota_override, 0)
`

func scanAccount(row interface {
	Scan(...any) error
},
) (platform.Account, error) {
	var a platform.Account
	var suspendedAt sql.NullTime
	if err := row.Scan(
		&a.ID,
		&a.Name,
		&a.Slug,
		&a.BillingEmail,
		&a.Tier,
		&a.Status,
		&a.CreatedAt,
		&suspendedAt,
		&a.SuspendedReason,
		&a.RateLimitPerMinOverride,
		&a.MonthlyRequestQuotaOverride,
	); err != nil {
		return platform.Account{}, err
	}
	if suspendedAt.Valid {
		a.SuspendedAt = suspendedAt.Time
	}
	// Stored rows carry the legacy five-string tier vocabulary; in memory the tier is always canonical, and [platform.Tier.StorageValue] reverses it on write.
	a.Tier = a.Tier.Canonical()
	return a, nil
}

// Create inserts a new account, mapping a slug collision to platform.ErrConflict; a slug an erasure retired (erased_account_slugs) collides too, because Redis keys and usage counters are keyed by acct:<slug> and reissuing it could hand a new owner leftovers.
func (r *AccountStore) Create(ctx context.Context, a platform.Account) (platform.Account, error) {
	const q = `
		INSERT INTO accounts (
			name, slug, billing_email,
			tier, status,
			rate_limit_per_min_override, monthly_request_quota_override
		)
		SELECT $1::text, $2::text, $3::citext, $4::text, $5::text,
		       NULLIF($6::int, 0), NULLIF($7::bigint, 0)
		 WHERE NOT EXISTS (
		       SELECT 1 FROM erased_account_slugs
		        WHERE slug_sha256 = sha256(convert_to($2::text, 'UTF8')))
		RETURNING ` + accountColumns

	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return platform.Account{}, fmt.Errorf("create account: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// A separate statement, so the INSERT's snapshot is taken after an
	// in-flight erasure of this slug has committed its tombstone.
	if _, err := tx.ExecContext(ctx, slugLockSQL, a.Slug); err != nil {
		return platform.Account{}, fmt.Errorf("create account: slug lock: %w", err)
	}
	row := tx.QueryRowContext(ctx, q,
		a.Name, a.Slug, a.BillingEmail,
		a.Tier.StorageValue(), string(a.Status),
		a.RateLimitPerMinOverride, a.MonthlyRequestQuotaOverride,
	)
	out, err := scanAccount(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.Is(err, sql.ErrNoRows) ||
			(errors.As(err, &pgErr) && pgErr.Code == pgErrUniqueViolation) {
			return platform.Account{}, fmt.Errorf("create account: %w", platform.ErrConflict)
		}
		return platform.Account{}, fmt.Errorf("create account: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return platform.Account{}, fmt.Errorf("create account: commit: %w", err)
	}
	return out, nil
}

// slugLockSQL serialises Create against EraseAccount on one slug; without it a Create started before the erasure commits waits on the old
// row's unique index, then inserts without seeing the tombstone.
const slugLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended('slug:' || $1::text, 0))`

// Get returns the account by ID; ErrNotFound if absent.
func (r *AccountStore) Get(ctx context.Context, id uuid.UUID) (platform.Account, error) {
	const q = `SELECT ` + accountColumns + ` FROM accounts WHERE id = $1`
	row := r.s.db.QueryRowContext(ctx, q, id)
	out, err := scanAccount(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.Account{}, platform.ErrNotFound
		}
		return platform.Account{}, fmt.Errorf("get account: %w", err)
	}
	return out, nil
}

// GetBySlug — same shape as Get; the slug column is UNIQUE.
func (r *AccountStore) GetBySlug(ctx context.Context, slug string) (platform.Account, error) {
	const q = `SELECT ` + accountColumns + ` FROM accounts WHERE slug = $1`
	row := r.s.db.QueryRowContext(ctx, q, slug)
	out, err := scanAccount(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.Account{}, platform.ErrNotFound
		}
		return platform.Account{}, fmt.Errorf("get account by slug: %w", err)
	}
	return out, nil
}

// Update writes the mutable fields incl. suspension bookkeeping; immutable fields (id, slug, created_at) are ignored so a Get, mutate, Update round-trip is safe.
func (r *AccountStore) Update(ctx context.Context, a platform.Account) error {
	// suspended_at/suspended_reason are written with status so a suspension is never enforced without a record of when or why; readers project both, so a round-trip rewrites what it read.
	n, err := execAccountUpdate(ctx, r.s.db, a)
	if err != nil {
		return fmt.Errorf("update account: %w", err)
	}
	if n == 0 {
		return platform.ErrNotFound
	}
	return nil
}

// accountUpdateQuery is the single UPDATE statement both [AccountStore.Update]
// and [AccountStore.UpdateAtomic] issue — kept as one literal so the two
// callers can never drift on which columns are written.
const accountUpdateQuery = `
	UPDATE accounts SET
		name = $2,
		billing_email = $3,
		tier = $4,
		status = $5,
		suspended_at = $6,
		suspended_reason = NULLIF($7, ''),
		rate_limit_per_min_override = NULLIF($8, 0),
		monthly_request_quota_override = NULLIF($9, 0)
	WHERE id = $1
`

// accountExecer is satisfied by both *sql.DB and *sql.Tx, so
// execAccountUpdate runs unchanged whether the caller is outside a
// transaction (Update) or inside one holding a row lock (UpdateAtomic).
type accountExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func execAccountUpdate(ctx context.Context, exec accountExecer, a platform.Account) (int64, error) {
	var suspendedAt any
	if !a.SuspendedAt.IsZero() {
		suspendedAt = a.SuspendedAt
	}
	res, err := exec.ExecContext(ctx, accountUpdateQuery,
		a.ID, a.Name, a.BillingEmail,
		a.Tier.StorageValue(), string(a.Status),
		suspendedAt, a.SuspendedReason,
		a.RateLimitPerMinOverride, a.MonthlyRequestQuotaOverride,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UpdateAtomic reads the row under SELECT ... FOR UPDATE, applies mutate and writes back in one transaction so racing PATCHes cannot lose a SUSPEND; an error from mutate aborts, ErrNotFound if absent.
func (r *AccountStore) UpdateAtomic(
	ctx context.Context, id uuid.UUID, mutate func(*platform.Account) error,
) (before, after platform.Account, err error) {
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return platform.Account{}, platform.Account{}, fmt.Errorf("update account atomic: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = $1 FOR UPDATE`, id)
	before, err = scanAccount(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.Account{}, platform.Account{}, platform.ErrNotFound
		}
		return platform.Account{}, platform.Account{}, fmt.Errorf("update account atomic: load: %w", err)
	}

	after = before
	if mErr := mutate(&after); mErr != nil {
		return platform.Account{}, platform.Account{}, mErr
	}

	if _, err := execAccountUpdate(ctx, tx, after); err != nil {
		return platform.Account{}, platform.Account{}, fmt.Errorf("update account atomic: write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return platform.Account{}, platform.Account{}, fmt.Errorf("update account atomic: commit: %w", err)
	}
	return before, after, nil
}

// Suspend sets status=suspended and the reason; idempotent, an already-suspended account just gets the reason rewritten.
func (r *AccountStore) Suspend(ctx context.Context, id uuid.UUID, reason string) error {
	const q = `
		UPDATE accounts SET
			status = 'suspended',
			suspended_at = COALESCE(suspended_at, now()),
			suspended_reason = $2
		WHERE id = $1
	`
	res, err := r.s.db.ExecContext(ctx, q, id, reason)
	if err != nil {
		return fmt.Errorf("suspend account: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return platform.ErrNotFound
	}
	return nil
}

// Unsuspend clears suspension state. Idempotent.
func (r *AccountStore) Unsuspend(ctx context.Context, id uuid.UUID) error {
	const q = `
		UPDATE accounts SET
			status = 'active',
			suspended_at = NULL,
			suspended_reason = NULL
		WHERE id = $1
	`
	res, err := r.s.db.ExecContext(ctx, q, id)
	if err != nil {
		return fmt.Errorf("unsuspend account: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return platform.ErrNotFound
	}
	return nil
}

// ReapSuspendedOrphans hard-deletes suspended signup-race orphans (reason starts with reasonPrefix, older than olderThan, NO users AND NO api_keys) and returns rows deleted; reasonPrefix LIKE metacharacters are escaped.
func (r *AccountStore) ReapSuspendedOrphans(ctx context.Context, reasonPrefix string, olderThan time.Time) (int64, error) {
	const q = `
		DELETE FROM accounts a
		WHERE a.id IN (
		    SELECT a2.id FROM accounts a2
		     WHERE a2.status = 'suspended'
		       AND a2.suspended_reason LIKE $1 ESCAPE '\'
		       AND a2.suspended_at IS NOT NULL
		       AND a2.suspended_at < $2
		       AND NOT EXISTS (SELECT 1 FROM users u WHERE u.account_id = a2.id)
		       AND NOT EXISTS (SELECT 1 FROM api_keys k WHERE k.account_id = a2.id)
		     LIMIT $3
		)
	`
	var total int64
	for i := 0; i < sweepMaxBatchesPerCall; i++ {
		res, err := r.s.db.ExecContext(ctx, q, likePrefixPattern(reasonPrefix), olderThan, r.sweepBatchRows)
		if err != nil {
			return total, fmt.Errorf("reap suspended orphans: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(r.sweepBatchRows) {
			break
		}
	}
	return total, nil
}

// ListAbandonedRegistrations returns up to limit accounts, oldest first, left by an unused /v1/register (active, free, no billing customer, no user, keys never used) created before createdBefore; erase them through [accounterasure.Eraser].
func (r *AccountStore) ListAbandonedRegistrations(ctx context.Context, createdBefore time.Time, limit int) ([]uuid.UUID, error) {
	const q = `
		SELECT a.id FROM accounts a
		 WHERE a.status = 'active'
		   AND a.tier = 'free'
		   AND a.stripe_customer_id IS NULL
		   AND a.created_at < $1
		   AND NOT EXISTS (SELECT 1 FROM users u WHERE u.account_id = a.id)
		   AND EXISTS (SELECT 1 FROM api_keys k WHERE k.account_id = a.id)
		   AND NOT EXISTS (SELECT 1 FROM api_keys k WHERE k.account_id = a.id AND k.last_used_at IS NOT NULL)
		 ORDER BY a.created_at
		 LIMIT $2
	`
	rows, err := r.s.db.QueryContext(ctx, q, createdBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("list abandoned registrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list abandoned registrations: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list abandoned registrations: %w", err)
	}
	return out, nil
}

// CountAccounts returns the row count of the accounts table, published by the signup-reaper sweep as [obs.AccountRows].
func (r *AccountStore) CountAccounts(ctx context.Context) (int64, error) {
	const q = `SELECT COUNT(*) FROM accounts`
	var n int64
	if err := r.s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("count accounts: %w", err)
	}
	return n, nil
}

// CountAPIKeys returns the row count of the api_keys table, published by the signup-reaper sweep as [obs.APIKeyRows].
func (r *AccountStore) CountAPIKeys(ctx context.Context) (int64, error) {
	const q = `SELECT COUNT(*) FROM api_keys`
	var n int64
	if err := r.s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("count api keys: %w", err)
	}
	return n, nil
}

// likePrefixPattern escapes LIKE metacharacters in p and appends `%`
// so the result matches "starts with p literally". Backslash is the
// ESCAPE char (Postgres default, made explicit in the query).
func likePrefixPattern(p string) string {
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(p)
	return esc + "%"
}

// Compile-time interface check.
var _ platform.AccountStore = (*AccountStore)(nil)
