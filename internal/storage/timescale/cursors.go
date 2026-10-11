package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Cursor is a per-source ingestion marker. Sub optionally distinguishes independent positions
// within one source (e.g. a pair contract ID versus "" for the factory cursor).
//
// FirstLedger is the earliest ledger the cursor's range covers, so the [FirstLedger, LastLedger]
// span only grows forward. It is zero while the column is NULL; density coverage then credits no
// live span instead of falling back to sourceGenesisLedger, which would inflate density to 100%.
type Cursor struct {
	Source      string
	Sub         string
	FirstLedger uint32
	LastLedger  uint32
	UpdatedAt   time.Time
}

// liveCursorSources are the ingestion_cursors namespaces holding LIVE resume state (the live
// indexer and per-domain projector positions), not records of finished one-shot jobs.
//
// One list, because reap-cursors (never DELETE these rows) and /v1/diagnostics/cursors (never
// classify them `abandoned`) must agree on which rows it covers: an old row here means ingest is
// STUCK, so it is the row an operator most needs to see. One-shot shard rows outlive their work
// by design.
var liveCursorSources = []string{"ledgerstream", "projector"}

// LiveCursorSources returns a copy of the live cursor namespaces.
func LiveCursorSources() []string { return slices.Clone(liveCursorSources) }

// IsLiveCursorSource reports whether an ingestion_cursors `source`
// names a live position rather than a one-shot job's shards.
func IsLiveCursorSource(source string) bool {
	return slices.Contains(liveCursorSources, source)
}

// GetCursor returns the stored cursor or ErrNotFound, which first-run callers treat as "start from
// the configured backfill ledger".
//
// first_ledger is read via COALESCE(..., 0), so NULL scans as FirstLedger=0. Callers that must
// tell "none persisted" from "covers ledger 0" use ListCursors with sourceGenesisLedger fallback.
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

// ListCursors returns every row in ingestion_cursors ordered by
// (source, sub_source). Used by diagnostic tooling — not a hot path.
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

// UpsertCursor stores the cursor, advancing any existing row for (source, sub).
//
// The DO UPDATE WHERE refuses to regress last_ledger (a lower-or-equal value is a silent no-op),
// guarding against callers without their own guard and two indexers racing in a bad deploy.
//
// first_ledger is set on INSERT as the lower-bound coverage anchor and intentionally preserved by
// COALESCE on UPDATE, so restarts never move it; a NULL is filled by the first UPDATE.
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

// CursorRead is the cursor state a long-cycle reader observed when its
// cycle STARTED — the "expected" half of [Store.AdvanceCursorFrom]'s
// compare-and-swap. Exists=false means GetCursor answered ErrNotFound (a
// source's first cycle); LastLedger is meaningless then.
type CursorRead struct {
	Exists     bool
	LastLedger uint32
}

// AdvanceCursorFrom advances (source, sub) to newLast ONLY IF the row still equals what the caller
// read (compare-and-swap). False with a nil error means the cursor moved: abandon and re-read.
//
// UpsertCursor's forward-only guard is wrong for the projector, whose write derives from a read up
// to PerSourceTimeout old: a [Store.RewindCursor] landing in the gap would be overwritten by the
// stale forward write and nothing re-projected. Comparing to the value READ makes the rewind win
// in any interleaving; under READ COMMITTED a parked advance re-evaluates `last_ledger = $3`.
//
// expected.Exists=false is the first-cycle seed (INSERT ... ON CONFLICT DO NOTHING). newLast must
// be strictly above expected.LastLedger; moving backward is RewindCursor's job.
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

// RewindCursor moves an existing cursor BACKWARD to lastLedger, the deliberate path that
// UpsertCursor's forward-only guard refuses. projector-replay uses it for historical
// re-projection; through UpsertCursor a rewind would match zero rows and silently write nothing.
//
// It errors if the row does not exist (rewinding a never-run source is operator error, not a seed
// path) and refuses to move FORWARD, since fast-forwarding skips data.
//
// It returns the ledger the row held when rewound, which can exceed the caller's earlier read if a
// projector cycle committed in between; the re-walked range runs up to it. The FOR UPDATE CTE
// makes that the latest committed value under READ COMMITTED, not the snapshot's.
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

// ReapCursors deletes ingestion_cursors rows with last_updated strictly older than cutoff, skipping
// sources in `protected` and, when `source` is non-empty, rows outside that source. It returns the
// rows deleted; reap-cursors previews, then applies with the same arguments and [LiveCursorSources].
//
// `protected` is a parameter so the SQL guard is testable against a list the test controls; the
// caller's Go-side planner applies the same exclusion and the two agreeing is the point.
//
// A cutoff-predicated DELETE is exact because last_updated only moves FORWARD: no row can enter
// the `< cutoff` set between preview and apply, and one that leaves it was just written to.
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
