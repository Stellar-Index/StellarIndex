package postgresstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/StellarIndex/stellar-index/internal/platform"
)

// StatusNoticeStore implements [platform.StatusNoticeStore] against
// the `status_notices` table from migration 0082 — the operator-posted
// customer-facing status banners.
type StatusNoticeStore struct{ s *Store }

// NewStatusNoticeStore returns the Postgres-backed implementation.
func NewStatusNoticeStore(s *Store) *StatusNoticeStore { return &StatusNoticeStore{s: s} }

// statusNoticeColumns centralises the SELECT projection so it stays in
// lock-step with scanStatusNotice.
const statusNoticeColumns = `
	id, title, body, severity, status,
	COALESCE(created_by, ''),
	created_at, updated_at, resolved_at
`

func scanStatusNotice(row interface {
	Scan(...any) error
},
) (platform.StatusNotice, error) {
	var (
		n          platform.StatusNotice
		severity   string
		status     string
		resolvedAt sql.NullTime
	)
	if err := row.Scan(
		&n.ID,
		&n.Title,
		&n.Body,
		&severity,
		&status,
		&n.CreatedBy,
		&n.CreatedAt,
		&n.UpdatedAt,
		&resolvedAt,
	); err != nil {
		return platform.StatusNotice{}, err
	}
	n.Severity = platform.StatusNoticeSeverity(severity)
	n.Status = platform.StatusNoticeStatus(status)
	if resolvedAt.Valid {
		n.ResolvedAt = resolvedAt.Time
	}
	return n, nil
}

// Create inserts a new notice. status is forced to active on insert —
// a notice is born active and only ever transitions to resolved via
// Resolve. The schema's CHECK constraints catch a malformed severity.
func (r *StatusNoticeStore) Create(ctx context.Context, n platform.StatusNotice) (platform.StatusNotice, error) {
	const q = `
		INSERT INTO status_notices (title, body, severity, status, created_by)
		VALUES ($1, $2, $3, 'active', NULLIF($4, ''))
		RETURNING ` + statusNoticeColumns

	row := r.s.db.QueryRowContext(ctx, q,
		n.Title, n.Body, string(n.Severity), n.CreatedBy)
	out, err := scanStatusNotice(row)
	if err != nil {
		return platform.StatusNotice{}, fmt.Errorf("create status notice: %w", err)
	}
	return out, nil
}

// Get returns the notice by ID; ErrNotFound if absent.
func (r *StatusNoticeStore) Get(ctx context.Context, id uuid.UUID) (platform.StatusNotice, error) {
	const q = `SELECT ` + statusNoticeColumns + ` FROM status_notices WHERE id = $1`
	row := r.s.db.QueryRowContext(ctx, q, id)
	out, err := scanStatusNotice(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.StatusNotice{}, platform.ErrNotFound
		}
		return platform.StatusNotice{}, fmt.Errorf("get status notice: %w", err)
	}
	return out, nil
}

// ListActive returns active notices newest-first (served by the
// partial index status_notices_active_idx).
func (r *StatusNoticeStore) ListActive(ctx context.Context) ([]platform.StatusNotice, error) {
	const q = `SELECT ` + statusNoticeColumns +
		` FROM status_notices WHERE status = 'active' ORDER BY created_at DESC`
	return r.queryNotices(ctx, q)
}

// List returns notices of any status, newest-first, capped at limit
// (default 100, hard-capped 500 to bound an operator's history page).
func (r *StatusNoticeStore) List(ctx context.Context, limit int) ([]platform.StatusNotice, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	const q = `SELECT ` + statusNoticeColumns +
		` FROM status_notices ORDER BY created_at DESC LIMIT $1`
	return r.queryNotices(ctx, q, limit)
}

func (r *StatusNoticeStore) queryNotices(ctx context.Context, q string, args ...any) ([]platform.StatusNotice, error) {
	rows, err := r.s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list status notices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]platform.StatusNotice, 0, 16)
	for rows.Next() {
		n, err := scanStatusNotice(rows)
		if err != nil {
			return nil, fmt.Errorf("scan status notice: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list status notices rows: %w", err)
	}
	return out, nil
}

// Resolve flips the notice to resolved, stamping resolved_at on the
// first transition only (COALESCE keeps the original timestamp on a
// repeat call — resolving is idempotent). Returns the updated row.
func (r *StatusNoticeStore) Resolve(ctx context.Context, id uuid.UUID) (platform.StatusNotice, error) {
	const q = `
		UPDATE status_notices SET
			status = 'resolved',
			resolved_at = COALESCE(resolved_at, now()),
			updated_at = now()
		WHERE id = $1
		RETURNING ` + statusNoticeColumns
	row := r.s.db.QueryRowContext(ctx, q, id)
	out, err := scanStatusNotice(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.StatusNotice{}, platform.ErrNotFound
		}
		return platform.StatusNotice{}, fmt.Errorf("resolve status notice: %w", err)
	}
	return out, nil
}

// Compile-time interface check.
var _ platform.StatusNoticeStore = (*StatusNoticeStore)(nil)
