package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ErrSourceExclusionLifted is returned by [Store.LiftSourceExclusion] for a row that was already lifted.
var ErrSourceExclusionLifted = errors.New("timescale: source exclusion already lifted")

// SourceExclusion is one source_exclusions row (migration 0231). Base and Quote are both empty for a
// source-wide exclusion, else the pair in canonical.Orient order; a zero Until is open-ended and a zero
// LiftedAt is not lifted.
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

// Lifted reports whether the row has been lifted.
func (e SourceExclusion) Lifted() bool { return !e.LiftedAt.IsZero() }

// Source-exclusion states as of a given time, as [SourceExclusion.State] reports them.
const (
	SourceExclusionPending = "pending"
	SourceExclusionInForce = "in-force"
	SourceExclusionExpired = "expired"
	SourceExclusionLifted  = "lifted"
)

// State reports the row's state at now: lifted, or else pending / in-force / expired against [From, Until).
func (e SourceExclusion) State(now time.Time) string {
	switch {
	case e.Lifted():
		return SourceExclusionLifted
	case now.Before(e.From):
		return SourceExclusionPending
	case !e.Until.IsZero() && !now.Before(e.Until):
		return SourceExclusionExpired
	}
	return SourceExclusionInForce
}

// Covers reports whether the un-lifted row excludes source's base/quote price at ts: ts in [From, Until)
// and the row is source-wide or names the pair in either direction under any canonical.AssetAliases form.
func (e SourceExclusion) Covers(source, base, quote string, ts time.Time) bool {
	return !e.Lifted() && e.Source == source && !ts.Before(e.From) &&
		(e.Until.IsZero() || ts.Before(e.Until)) && e.CoversPair(base, quote)
}

// CoversSpan is [SourceExclusion.Covers] for a value pooled over [from, to): true when the two windows overlap.
func (e SourceExclusion) CoversSpan(source, base, quote string, from, to time.Time) bool {
	return !e.Lifted() && e.Source == source && e.From.Before(to) &&
		(e.Until.IsZero() || e.Until.After(from)) && e.CoversPair(base, quote)
}

// CoversPair reports whether the row's pair scope includes base/quote, ignoring source, window and lift.
func (e SourceExclusion) CoversPair(base, quote string) bool {
	if e.Base == "" {
		return true
	}
	return (sameAssetFamily(e.Base, base) && sameAssetFamily(e.Quote, quote)) ||
		(sameAssetFamily(e.Base, quote) && sameAssetFamily(e.Quote, base))
}

func sameAssetFamily(stored, asked string) bool {
	if stored == asked {
		return true
	}
	a, err := canonical.ParseAsset(asked)
	if err != nil {
		return false
	}
	return slices.Contains(canonical.AssetAliasStrings(a), stored)
}

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

// LiftSourceExclusion stamps the lift columns of an un-lifted row; the row is kept. An unknown id returns
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

// UnliftedSourceExclusions returns every un-lifted row, including ones whose window has ended or not yet begun.
func (s *Store) UnliftedSourceExclusions(ctx context.Context) ([]SourceExclusion, error) {
	return s.ListSourceExclusions(ctx, true)
}

// ListSourceExclusions returns rows in id order, only the un-lifted ones when unliftedOnly.
func (s *Store) ListSourceExclusions(ctx context.Context, unliftedOnly bool) ([]SourceExclusion, error) {
	const q = `
		SELECT id, source, COALESCE(base_asset, ''), COALESCE(quote_asset, ''), from_ts, until_ts,
		       reason, actor, created_at, lifted_at, COALESCE(lifted_by, ''), COALESCE(lift_reason, '')
		  FROM source_exclusions
		 WHERE NOT $1 OR lifted_at IS NULL
		 ORDER BY id
	`
	rows, err := s.db.QueryContext(ctx, q, unliftedOnly)
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
