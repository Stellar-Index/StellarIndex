package postgresstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/StellarIndex/stellar-index/internal/platform"
)

// PriceAlertStore implements [platform.PriceAlertStore] against the
// `price_alerts` table from migration 0080 (BACKLOG #60).
//
// Shape mirrors [WebhookStore]: an atomic per-account cap on create
// (advisory lock + CTE-gated INSERT), owner-scoped reads for the
// dashboard, and an enabled-only sweep read for the aggregator's
// evaluator.
type PriceAlertStore struct{ s *Store }

// NewPriceAlertStore returns the Postgres-backed implementation.
func NewPriceAlertStore(s *Store) *PriceAlertStore {
	return &PriceAlertStore{s: s}
}

// Compile-time interface conformance.
var _ platform.PriceAlertStore = (*PriceAlertStore)(nil)

// CreatePriceAlert inserts the alert, enforcing the per-account
// `maxPerAccount` cap atomically. Same race-proof shape as
// [WebhookStore.CreateWebhook] (F-1248): a per-account advisory lock
// serialises concurrent creates, and the count + insert observe a
// stable view via `WHERE current_count.n < $N`. Returns
// [platform.ErrPriceAlertQuotaExceeded] when the account is at the cap.
func (c *PriceAlertStore) CreatePriceAlert(ctx context.Context, a platform.PriceAlert, maxPerAccount int) (platform.PriceAlert, error) {
	if a.AccountID == uuid.Nil {
		return platform.PriceAlert{}, errors.New("postgresstore: CreatePriceAlert: AccountID is empty")
	}
	if a.BaseAsset == "" || a.QuoteAsset == "" {
		return platform.PriceAlert{}, errors.New("postgresstore: CreatePriceAlert: base/quote asset is empty")
	}
	if !platform.ValidAlertCondition(string(a.Condition)) {
		return platform.PriceAlert{}, fmt.Errorf("postgresstore: CreatePriceAlert: invalid condition %q", a.Condition)
	}
	if a.Threshold == "" {
		return platform.PriceAlert{}, errors.New("postgresstore: CreatePriceAlert: Threshold is empty")
	}
	if maxPerAccount <= 0 {
		maxPerAccount = 5
	}
	tx, err := c.s.db.BeginTx(ctx, nil)
	if err != nil {
		return platform.PriceAlert{}, fmt.Errorf("postgresstore: CreatePriceAlert: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Per-account advisory lock (namespaced 'pricealert:' so it does
	// not collide with the webhook lock) inside the transaction.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('pricealert:' || $1::text))`,
		a.AccountID); err != nil {
		return platform.PriceAlert{}, fmt.Errorf("postgresstore: CreatePriceAlert: advisory lock: %w", err)
	}

	const q = `
		WITH current_count AS (
		    SELECT COUNT(*) AS n
		      FROM price_alerts
		     WHERE account_id = $1
		)
		INSERT INTO price_alerts
		    (account_id, base_asset, quote_asset, condition, threshold, cooldown_seconds, enabled)
		SELECT $1, $2, $3, $4, $5, $6, $7
		  FROM current_count
		 WHERE current_count.n < $8
		RETURNING id, created_at, updated_at
	`
	row := tx.QueryRowContext(ctx, q,
		a.AccountID, a.BaseAsset, a.QuoteAsset, string(a.Condition),
		a.Threshold, a.CooldownSeconds, a.Enabled, maxPerAccount,
	)
	if err := row.Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.PriceAlert{}, platform.ErrPriceAlertQuotaExceeded
		}
		return platform.PriceAlert{}, fmt.Errorf("postgresstore: CreatePriceAlert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return platform.PriceAlert{}, fmt.Errorf("postgresstore: CreatePriceAlert: commit: %w", err)
	}
	return a, nil
}

// GetPriceAlert returns one alert by ID. [platform.ErrNotFound] when
// absent.
func (c *PriceAlertStore) GetPriceAlert(ctx context.Context, id uuid.UUID) (platform.PriceAlert, error) {
	const q = `
		SELECT id, account_id, base_asset, quote_asset, condition, threshold,
		       cooldown_seconds, enabled,
		       COALESCE(last_fired_at, '0001-01-01 00:00:00+00'::timestamptz),
		       created_at, updated_at
		  FROM price_alerts
		 WHERE id = $1
	`
	row := c.s.db.QueryRowContext(ctx, q, id)
	a, err := scanPriceAlertRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return platform.PriceAlert{}, platform.ErrNotFound
	}
	return a, err
}

// ListPriceAlertsForAccount returns every alert the account owns,
// newest first.
func (c *PriceAlertStore) ListPriceAlertsForAccount(ctx context.Context, accountID uuid.UUID) ([]platform.PriceAlert, error) {
	const q = `
		SELECT id, account_id, base_asset, quote_asset, condition, threshold,
		       cooldown_seconds, enabled,
		       COALESCE(last_fired_at, '0001-01-01 00:00:00+00'::timestamptz),
		       created_at, updated_at
		  FROM price_alerts
		 WHERE account_id = $1
		 ORDER BY created_at DESC
	`
	return c.queryAlerts(ctx, "ListPriceAlertsForAccount", q, accountID)
}

// ListEnabledPriceAlerts returns every enabled alert across all
// accounts. The evaluator sweeps this set each tick; the partial index
// `price_alerts_enabled_idx` keeps the scan tight.
func (c *PriceAlertStore) ListEnabledPriceAlerts(ctx context.Context) ([]platform.PriceAlert, error) {
	const q = `
		SELECT id, account_id, base_asset, quote_asset, condition, threshold,
		       cooldown_seconds, enabled,
		       COALESCE(last_fired_at, '0001-01-01 00:00:00+00'::timestamptz),
		       created_at, updated_at
		  FROM price_alerts
		 WHERE enabled = TRUE
		 ORDER BY account_id, created_at
	`
	return c.queryAlerts(ctx, "ListEnabledPriceAlerts", q)
}

// UpdatePriceAlert persists the mutable fields. AccountID + ID are
// immutable post-create. [platform.ErrNotFound] when the row is gone.
func (c *PriceAlertStore) UpdatePriceAlert(ctx context.Context, a platform.PriceAlert) error {
	if a.ID == uuid.Nil {
		return errors.New("postgresstore: UpdatePriceAlert: ID is empty")
	}
	if !platform.ValidAlertCondition(string(a.Condition)) {
		return fmt.Errorf("postgresstore: UpdatePriceAlert: invalid condition %q", a.Condition)
	}
	const q = `
		UPDATE price_alerts
		   SET base_asset       = $2,
		       quote_asset      = $3,
		       condition        = $4,
		       threshold        = $5,
		       cooldown_seconds = $6,
		       enabled          = $7,
		       updated_at       = now()
		 WHERE id = $1
	`
	res, err := c.s.db.ExecContext(ctx, q,
		a.ID, a.BaseAsset, a.QuoteAsset, string(a.Condition),
		a.Threshold, a.CooldownSeconds, a.Enabled,
	)
	if err != nil {
		return fmt.Errorf("postgresstore: UpdatePriceAlert %s: %w", a.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgresstore: UpdatePriceAlert %s rows affected: %w", a.ID, err)
	}
	if n == 0 {
		return platform.ErrNotFound
	}
	return nil
}

// DeletePriceAlert removes the row. Idempotent.
func (c *PriceAlertStore) DeletePriceAlert(ctx context.Context, id uuid.UUID) error {
	const q = `DELETE FROM price_alerts WHERE id = $1`
	if _, err := c.s.db.ExecContext(ctx, q, id); err != nil {
		return fmt.Errorf("postgresstore: DeletePriceAlert %s: %w", id, err)
	}
	return nil
}

// MarkPriceAlertFired stamps last_fired_at (+ bumps updated_at).
func (c *PriceAlertStore) MarkPriceAlertFired(ctx context.Context, id uuid.UUID, firedAt time.Time) error {
	const q = `
		UPDATE price_alerts
		   SET last_fired_at = $2,
		       updated_at    = now()
		 WHERE id = $1
	`
	if _, err := c.s.db.ExecContext(ctx, q, id, firedAt.UTC()); err != nil {
		return fmt.Errorf("postgresstore: MarkPriceAlertFired %s: %w", id, err)
	}
	return nil
}

// ─── helpers ────────────────────────────────────────────────────

func (c *PriceAlertStore) queryAlerts(ctx context.Context, op, q string, args ...any) ([]platform.PriceAlert, error) {
	rows, err := c.s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("postgresstore: %s: %w", op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []platform.PriceAlert
	for rows.Next() {
		a, err := scanPriceAlertRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgresstore: %s rows: %w", op, err)
	}
	return out, nil
}

func scanPriceAlertRow(s rowScanner) (platform.PriceAlert, error) {
	var (
		a         platform.PriceAlert
		condition string
	)
	if err := s.Scan(
		&a.ID, &a.AccountID, &a.BaseAsset, &a.QuoteAsset, &condition,
		&a.Threshold, &a.CooldownSeconds, &a.Enabled,
		&a.LastFiredAt, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return platform.PriceAlert{}, fmt.Errorf("postgresstore: scan price alert: %w", err)
	}
	a.Condition = platform.AlertCondition(condition)
	// COALESCE pushed the sentinel zero-time for a NULL last_fired_at;
	// translate it back to Go's zero time.Time so callers use IsZero().
	if a.LastFiredAt.Year() == 1 {
		a.LastFiredAt = time.Time{}
	}
	return a, nil
}
