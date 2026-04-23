package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Cursor is a per-source ingestion marker. Sub is an optional
// differentiator for sources that track multiple positions
// independently (e.g. Soroswap tracks factory events + per-pair
// events separately; Soroswap's consumer.go sets Sub to the pair's
// contract ID for pair cursors, "" for the factory cursor).
type Cursor struct {
	Source     string
	Sub        string
	LastLedger uint32
	UpdatedAt  time.Time
}

// GetCursor returns the stored cursor or ErrNotFound. Callers on
// first run typically translate ErrNotFound to "start from
// configured backfill-from-ledger" rather than an error condition.
func (s *Store) GetCursor(ctx context.Context, source, sub string) (Cursor, error) {
	const q = `
        SELECT source, COALESCE(sub_source, ''), last_ledger, last_updated
          FROM ingestion_cursors
         WHERE source = $1 AND sub_source = $2
    `
	var c Cursor
	err := s.db.QueryRowContext(ctx, q, source, sub).Scan(
		&c.Source, &c.Sub, &c.LastLedger, &c.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Cursor{}, ErrNotFound
	}
	if err != nil {
		return Cursor{}, fmt.Errorf("timescale: GetCursor: %w", err)
	}
	return c, nil
}

// ListCursors returns every row in ingestion_cursors ordered by
// (source, sub_source). Used by diagnostic tooling — not a hot path.
func (s *Store) ListCursors(ctx context.Context) ([]Cursor, error) {
	const q = `
        SELECT source, COALESCE(sub_source, ''), last_ledger, last_updated
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
		if err := rows.Scan(&c.Source, &c.Sub, &c.LastLedger, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("timescale: ListCursors scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListCursors rows: %w", err)
	}
	return out, nil
}

// UpsertCursor stores the cursor, replacing any existing row for
// (source, sub). The last_updated column is server-side `now()`.
func (s *Store) UpsertCursor(ctx context.Context, source, sub string, lastLedger uint32) error {
	const q = `
        INSERT INTO ingestion_cursors (source, sub_source, last_ledger, last_updated)
        VALUES ($1, $2, $3, now())
        ON CONFLICT (source, sub_source)
        DO UPDATE SET last_ledger  = EXCLUDED.last_ledger,
                      last_updated = EXCLUDED.last_updated
    `
	_, err := s.db.ExecContext(ctx, q, source, sub, lastLedger)
	if err != nil {
		return fmt.Errorf("timescale: UpsertCursor: %w", err)
	}
	return nil
}
