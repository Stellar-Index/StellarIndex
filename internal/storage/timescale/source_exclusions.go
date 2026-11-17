package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrSourceExclusionLifted is returned by [Store.LiftSourceExclusion] for a row that was already lifted.
var ErrSourceExclusionLifted = errors.New("timescale: source exclusion already lifted")

// SourceExclusion is one source_exclusions row (migration 0231). Base and Quote are both empty for a
// source-wide exclusion; a zero Until is open-ended and a zero LiftedAt is still active.
type SourceExclusion struct {
	ID         int64
	Source     string
	Base       string
	Quote      string
	From       time.Time
	Until      time.Time
	Reason     string
	Actor      string
	CreatedAt  time.Time
	LiftedAt   time.Time
	LiftedBy   string
	LiftReason string
}

// Active reports whether the row has not been lifted.
func (e SourceExclusion) Active() bool { return e.LiftedAt.IsZero() }

// InsertSourceExclusion appends e and returns its id; ID, CreatedAt and the lift fields of e are ignored.
func (s *Store) InsertSourceExclusion(ctx context.Context, e SourceExclusion) (int64, error) {
	const q = `
		INSERT INTO source_exclusions (source, base_asset, quote_asset, from_ts, until_ts, reason, actor)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), $4, $5, $6, $7)
		RETURNING id
	`
	var until sql.NullTime
	if !e.Until.IsZero() {
		until = sql.NullTime{Time: e.Until.UTC(), Valid: true}
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, q,
		e.Source, e.Base, e.Quote, e.From.UTC(), until, e.Reason, e.Actor,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("timescale: InsertSourceExclusion %s: %w", e.Source, err)
	}
	return id, nil
}

// LiftSourceExclusion stamps the lift columns of an active row; the row is kept. An unknown id returns
// ErrNotFound and an already-lifted one ErrSourceExclusionLifted.
func (s *Store) LiftSourceExclusion(ctx context.Context, id int64, liftedBy, liftReason string) error {
	const q = `
		UPDATE source_exclusions
		   SET lifted_at = now(), lifted_by = $2, lift_reason = $3
		 WHERE id = $1 AND lifted_at IS NULL
	`
	res, err := s.db.ExecContext(ctx, q, id, liftedBy, liftReason)
	if err != nil {
		return fmt.Errorf("timescale: LiftSourceExclusion %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("timescale: LiftSourceExclusion %d: rows affected: %w", id, err)
	}
	if n > 0 {
		return nil
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM source_exclusions WHERE id = $1)`, id,
	).Scan(&exists); err != nil {
		return fmt.Errorf("timescale: LiftSourceExclusion %d: existence check: %w", id, err)
	}
	if exists {
		return fmt.Errorf("source exclusion %d: %w", id, ErrSourceExclusionLifted)
	}
	return fmt.Errorf("source exclusion %d: %w", id, ErrNotFound)
}

// ActiveSourceExclusions returns every un-lifted row, including ones whose window has ended or not yet begun.
func (s *Store) ActiveSourceExclusions(ctx context.Context) ([]SourceExclusion, error) {
	return s.ListSourceExclusions(ctx, true)
}

// ListSourceExclusions returns rows in id order, only the un-lifted ones when activeOnly.
func (s *Store) ListSourceExclusions(ctx context.Context, activeOnly bool) ([]SourceExclusion, error) {
	const q = `
		SELECT id, source, COALESCE(base_asset, ''), COALESCE(quote_asset, ''), from_ts, until_ts,
		       reason, actor, created_at, lifted_at, COALESCE(lifted_by, ''), COALESCE(lift_reason, '')
		  FROM source_exclusions
		 WHERE NOT $1 OR lifted_at IS NULL
		 ORDER BY id
	`
	rows, err := s.db.QueryContext(ctx, q, activeOnly)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListSourceExclusions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SourceExclusion
	for rows.Next() {
		var (
			e               SourceExclusion
			until, liftedAt sql.NullTime
		)
		if err := rows.Scan(&e.ID, &e.Source, &e.Base, &e.Quote, &e.From, &until,
			&e.Reason, &e.Actor, &e.CreatedAt, &liftedAt, &e.LiftedBy, &e.LiftReason); err != nil {
			return nil, fmt.Errorf("timescale: ListSourceExclusions scan: %w", err)
		}
		if until.Valid {
			e.Until = until.Time
		}
		if liftedAt.Valid {
			e.LiftedAt = liftedAt.Time
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListSourceExclusions rows: %w", err)
	}
	return out, nil
}
