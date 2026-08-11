package postgresstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// AccountStore implements [platform.AccountStore] against
// Postgres. Constructor takes a [*Store] so multiple platform
// stores share the connection pool.
type AccountStore struct{ s *Store }

// NewAccountStore returns the Postgres-backed implementation.
func NewAccountStore(s *Store) *AccountStore { return &AccountStore{s: s} }

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
	// Legacy-tier folding (free-platform model, 2026-08-11): stored
	// rows still carry the migration-0027 five-string vocabulary
	// (free/starter/pro/business/enterprise). In-memory the tier is
	// always canonical (free/partner) so every ladder lookup, clamp,
	// and wire view speaks the three-level model; the reverse mapping
	// happens at write time via [platform.Tier.StorageValue].
	a.Tier = a.Tier.Canonical()
	return a, nil
}

// Create inserts a new account. The schema's CHECK constraints
// catch malformed slugs / tiers / statuses; we map the unique-
// violation case (slug collision) to platform.ErrConflict.
func (r *AccountStore) Create(ctx context.Context, a platform.Account) (platform.Account, error) {
	const q = `
		INSERT INTO accounts (
			name, slug, billing_email,
			tier, status,
			rate_limit_per_min_override, monthly_request_quota_override
		)
		VALUES ($1, $2, $3, $4, $5,
		        NULLIF($6, 0), NULLIF($7, 0))
		RETURNING ` + accountColumns

	row := r.s.db.QueryRowContext(ctx, q,
		a.Name, a.Slug, a.BillingEmail,
		a.Tier.StorageValue(), string(a.Status),
		a.RateLimitPerMinOverride, a.MonthlyRequestQuotaOverride,
	)
	out, err := scanAccount(row)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == pgErrUniqueViolation {
			return platform.Account{}, fmt.Errorf("create account: %w", platform.ErrConflict)
		}
		return platform.Account{}, fmt.Errorf("create account: %w", err)
	}
	return out, nil
}

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

// Update writes the mutable fields — including the suspension
// bookkeeping (status, suspended_at, suspended_reason). Immutable (id,
// slug, created_at) are ignored; passing different values is a no-op
// rather than an error so callers can round-trip a Get → mutate →
// Update pattern.
func (r *AccountStore) Update(ctx context.Context, a platform.Account) error {
	// suspended_at / suspended_reason are written here too (C3-010,
	// audit-2026-07-23). Pre-fix Update wrote `status` but silently
	// dropped the two columns that explain it, so a caller doing the
	// documented Get → mutate → Update round-trip could move an account
	// to `suspended` and leave suspended_at NULL with no reason — the
	// suspension would be enforced with no record of when or why. Every
	// reader projects both columns (accountColumns), so an untouched
	// round-trip rewrites the same values it read.
	const q = `
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
	var suspendedAt any
	if !a.SuspendedAt.IsZero() {
		suspendedAt = a.SuspendedAt
	}
	res, err := r.s.db.ExecContext(ctx, q,
		a.ID, a.Name, a.BillingEmail,
		a.Tier.StorageValue(), string(a.Status),
		suspendedAt, a.SuspendedReason,
		a.RateLimitPerMinOverride, a.MonthlyRequestQuotaOverride,
	)
	if err != nil {
		return fmt.Errorf("update account: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return platform.ErrNotFound
	}
	return nil
}

// Suspend sets status=suspended + reason. Idempotent — calling
// on an already-suspended account just rewrites the reason.
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

// ReapSuspendedOrphans hard-deletes speculative-account orphans: rows
// that were Suspended with a `suspended_reason` starting with
// reasonPrefix (the `signup-race:` marker the F-1255 lost-race
// recovery path stamps on the losing account) and suspended strictly
// before olderThan.
//
// The predicate is deliberately conservative. Beyond the exact-reason
// + age-threshold match it additionally requires the account to have
// NO child users AND NO api_keys. A lost signup-race orphan has
// neither by construction — its `CreateUser` lost the `users_email_idx`
// race, so no user (and therefore no dashboard key) was ever attached.
// The NOT EXISTS guards make the delete self-evidently safe for a
// human reviewer AND sidestep the `users` / `api_keys`
// `ON DELETE RESTRICT` foreign keys (migration 0027), which would
// otherwise abort the whole batch if a single row were ever
// mislabelled. Returns the number of rows deleted.
//
// reasonPrefix is matched with `LIKE '<escaped-prefix>%'`; callers
// pass a fixed package constant (signupreaper.SignupRaceReasonPrefix),
// and any LIKE metacharacters in it are escaped, so there is no
// pattern-injection surface.
func (r *AccountStore) ReapSuspendedOrphans(ctx context.Context, reasonPrefix string, olderThan time.Time) (int64, error) {
	const q = `
		DELETE FROM accounts a
		WHERE a.status = 'suspended'
		  AND a.suspended_reason LIKE $1 ESCAPE '\'
		  AND a.suspended_at IS NOT NULL
		  AND a.suspended_at < $2
		  AND NOT EXISTS (SELECT 1 FROM users u WHERE u.account_id = a.id)
		  AND NOT EXISTS (SELECT 1 FROM api_keys k WHERE k.account_id = a.id)
	`
	res, err := r.s.db.ExecContext(ctx, q, likePrefixPattern(reasonPrefix), olderThan)
	if err != nil {
		return 0, fmt.Errorf("reap suspended orphans: %w", err)
	}
	n, _ := res.RowsAffected()
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
