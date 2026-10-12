package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Cursor is a per-source ingestion marker; Sub separates independent positions within a source, and FirstLedger (0 while NULL) is the earliest ledger covered so the span only grows forward.
type Cursor struct {
	Source      string
	Sub         string
	FirstLedger uint32
	LastLedger  uint32
	UpdatedAt   time.Time
}

// liveCursorSources are the ingestion_cursors namespaces holding LIVE resume state, not finished one-shot jobs. One list so reap-cursors
// (never DELETE these) and /v1/diagnostics/cursors (never `abandoned`) agree: an old row here means ingest is STUCK.
var liveCursorSources = []string{"ledgerstream", "projector"}

// LiveCursorSources returns a copy of the live cursor namespaces.
func LiveCursorSources() []string { return slices.Clone(liveCursorSources) }

// IsLiveCursorSource reports whether an ingestion_cursors `source` names a live position rather than a one-shot job's shards.
func IsLiveCursorSource(source string) bool {
	return slices.Contains(liveCursorSources, source)
}

// GetCursor returns the stored cursor or ErrNotFound; first_ledger is COALESCEd to 0, so callers that must tell "none persisted" from "covers ledger 0" use ListCursors.
func (s *Store) GetCursor(ctx context.Context, source, sub string) (Cursor, error) {
	const q = `
        SELECT source, COALESCE(sub_source, ''),
               COALESCE(first_ledger, 0), last_ledger, last_updated
          FROM ingestion_cursors
         WHERE source = $1 AND sub_source = $2
    `
	var c Cursor
	err := s.db.QueryRowContext(ctx, q, source, sub).Scan(
		&c.Source, &c.Sub, &c.FirstLedger, &c.LastLedger, &c.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Cursor{}, ErrNotFound
	}
	if err != nil {
		return Cursor{}, fmt.Errorf("timescale: GetCursor: %w", err)
	}
	return c, nil
}

// ListCursors returns every ingestion_cursors row ordered by (source, sub_source); diagnostics only, not a hot path.
func (s *Store) ListCursors(ctx context.Context) ([]Cursor, error) {
	const q = `
        SELECT source, COALESCE(sub_source, ''),
               COALESCE(first_ledger, 0), last_ledger, last_updated
          FROM ingestion_cursors
         ORDER BY source ASC, sub_source ASC
    `
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListCursors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Cursor
	for rows.Next() {
		var c Cursor
		if err := rows.Scan(&c.Source, &c.Sub, &c.FirstLedger, &c.LastLedger, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("timescale: ListCursors scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListCursors rows: %w", err)
	}
	return out, nil
}

// UpsertCursor stores the cursor, advancing an existing row; the DO UPDATE WHERE refuses to regress last_ledger (lower-or-equal is a silent no-op) and first_ledger is set on INSERT and preserved on UPDATE.
func (s *Store) UpsertCursor(ctx context.Context, source, sub string, lastLedger uint32) error {
	const q = `
        INSERT INTO ingestion_cursors (source, sub_source, first_ledger, last_ledger, last_updated)
        VALUES ($1, $2, $3, $3, now())
        ON CONFLICT (source, sub_source)
        DO UPDATE SET first_ledger = COALESCE(ingestion_cursors.first_ledger, EXCLUDED.first_ledger),
                      last_ledger  = EXCLUDED.last_ledger,
                      last_updated = EXCLUDED.last_updated
         WHERE EXCLUDED.last_ledger > ingestion_cursors.last_ledger
    `
	_, err := s.db.ExecContext(ctx, q, source, sub, lastLedger)
	if err != nil {
		return fmt.Errorf("timescale: UpsertCursor: %w", err)
	}
	return nil
}

// CursorRead is the cursor state a long-cycle reader observed when its cycle STARTED — the "expected" half of [Store.AdvanceCursorFrom]'s compare-and-swap.
type CursorRead struct {
	Exists     bool
	LastLedger uint32
}

// AdvanceCursorFrom advances (source, sub) to newLast only if the row still equals what the caller read (compare-and-swap); false with nil error means it moved, so abandon and re-read.
func (s *Store) AdvanceCursorFrom(ctx context.Context, source, sub string, expected CursorRead, newLast uint32) (bool, error) {
	var (
		res sql.Result
		err error
	)
	if expected.Exists {
		if newLast <= expected.LastLedger {
			return false, fmt.Errorf("timescale: AdvanceCursorFrom (%s,%s): %d is not an advance over the read position %d", source, sub, newLast, expected.LastLedger)
		}
		const q = `
        UPDATE ingestion_cursors
           SET first_ledger = COALESCE(first_ledger, $4),
               last_ledger  = $4,
               last_updated = now()
         WHERE source = $1 AND sub_source = $2 AND last_ledger = $3
    `
		res, err = s.db.ExecContext(ctx, q, source, sub, int64(expected.LastLedger), int64(newLast))
	} else {
		const q = `
        INSERT INTO ingestion_cursors (source, sub_source, first_ledger, last_ledger, last_updated)
        VALUES ($1, $2, $3, $3, now())
        ON CONFLICT (source, sub_source) DO NOTHING
    `
		res, err = s.db.ExecContext(ctx, q, source, sub, int64(newLast))
	}
	if err != nil {
		return false, fmt.Errorf("timescale: AdvanceCursorFrom (%s,%s): %w", source, sub, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("timescale: AdvanceCursorFrom (%s,%s) rows: %w", source, sub, err)
	}
	return n > 0, nil
}

// RewindCursor moves an existing cursor BACKWARD to lastLedger, the deliberate path UpsertCursor's forward-only guard refuses; errors if the row does not exist.
func (s *Store) RewindCursor(ctx context.Context, source, sub string, lastLedger uint32) (uint32, error) {
	const q = `
        WITH prior AS (
            SELECT last_ledger FROM ingestion_cursors
             WHERE source = $1 AND sub_source = $2 AND last_ledger > $3
               FOR UPDATE
        )
        UPDATE ingestion_cursors c
           SET last_ledger = $3, last_updated = now()
          FROM prior
         WHERE c.source = $1 AND c.sub_source = $2 AND c.last_ledger > $3
        RETURNING prior.last_ledger
    `
	var prior int64
	err := s.db.QueryRowContext(ctx, q, source, sub, lastLedger).Scan(&prior)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("timescale: RewindCursor (%s,%s): no row rewound — cursor missing or already at/below ledger %d", source, sub, lastLedger)
	}
	if err != nil {
		return 0, fmt.Errorf("timescale: RewindCursor: %w", err)
	}
	return uint32(prior), nil //nolint:gosec // ledger seq, bounded by the network head
}

// ReapCursors deletes ingestion_cursors rows with last_updated older than cutoff, skipping sources in `protected` and, if `source` is set, other sources; returns the rows deleted.
func (s *Store) ReapCursors(ctx context.Context, cutoff time.Time, source string, protected []string) (int64, error) {
	const q = `
        DELETE FROM ingestion_cursors
         WHERE last_updated < $1
           AND ($2 = '' OR source = $2)
           AND source <> ALL($3)
    `
	if protected == nil {
		protected = []string{}
	}
	res, err := s.db.ExecContext(ctx, q, cutoff.UTC(), source, protected)
	if err != nil {
		return 0, fmt.Errorf("timescale: ReapCursors: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("timescale: ReapCursors rows: %w", err)
	}
	return n, nil
}
