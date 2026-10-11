package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Cursor is a per-source ingestion marker. Sub is an optional
// differentiator for sources that track multiple positions
// independently (e.g. Soroswap tracks factory events + per-pair
// events separately; Soroswap's consumer.go sets Sub to the pair's
// contract ID for pair cursors, "" for the factory cursor).
//
// FirstLedger is the earliest ledger this cursor's range covers.
// For backfill cursors it is the `from` end of the assigned range
// (also embedded in Sub as "<from>-<to>:<decoders>"). For the live
// ledgerstream cursor it is the first ledger the live indexer
// ingested in this region — populated on the first INSERT via
// UpsertCursor and COALESCE-populated on the first UPDATE if a
// pre-migration-0046 NULL row exists. Preserved by ON CONFLICT
// DO UPDATE on every subsequent advance so the live cursor's
// [FirstLedger, LastLedger] coverage span only grows forward.
// Zero when the column is NULL on disk AND no UPDATE has yet
// flipped it (the seconds between deploy and the first live
// tick on a freshly-migrated cluster). The density-coverage
// projection declines to credit any live span in that transient
// window — honest about "we don't yet know how far back this
// cursor reaches" — rather than falling back to
// sourceGenesisLedger, which would silently inflate density to
// 100% for sources whose live cursor stayed NULL. See migration 0046
// + UpsertCursor.
type Cursor struct {
	Source      string
	Sub         string
	FirstLedger uint32
	LastLedger  uint32
	UpdatedAt   time.Time
}

// liveCursorSources are the ingestion_cursors `source` namespaces that
// hold LIVE resume state — a position something is still expected to
// advance — rather than a record of a one-shot job that has ended.
// `ledgerstream` is the live indexer's position (cmd/stellarindex-indexer)
// and `projector` the ADR-0032 per-domain projection position.
//
// One list, because two consumers draw opposite conclusions from the
// same fact and must not disagree about which rows it covers:
// `stellarindex-ops reap-cursors` refuses to DELETE these rows at any
// age, and /v1/diagnostics/cursors refuses to classify them
// `abandoned`. Both follow from one property — an old row here means
// ingest is STUCK, an incident, so it is the row an operator most needs
// to see and least wants deleted. A sharded one-shot job's namespace
// has the opposite property: its rows outlive the work by design.
var liveCursorSources = []string{"ledgerstream", "projector"}

// LiveCursorSources returns a copy of the live cursor namespaces.
func LiveCursorSources() []string { return slices.Clone(liveCursorSources) }

// IsLiveCursorSource reports whether an ingestion_cursors `source`
// names a live position rather than a one-shot job's shards.
func IsLiveCursorSource(source string) bool {
	return slices.Contains(liveCursorSources, source)
}

// GetCursor returns the stored cursor or ErrNotFound. Callers on
// first run typically translate ErrNotFound to "start from
// configured backfill-from-ledger" rather than an error condition.
//
// first_ledger is read via COALESCE(..., 0) so a NULL column on a
// pre-migration-0046 row scans cleanly as FirstLedger=0. Callers
// distinguishing "no first_ledger persisted" from "covers ledger 0"
// MUST use ListCursors + sourceGenesisLedger fallback semantics
// (the density-projection path); GetCursor's zero is unambiguous
// for non-zero-genesis sources.
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

// UpsertCursor stores the cursor, advancing any existing row for
// (source, sub). The last_updated column is server-side `now()`.
//
// Monotonic-advance guard: the `WHERE` on DO UPDATE refuses to regress
// last_ledger, so a lower-or-equal value is a silent no-op. This is
// defense-in-depth against a caller that forgot its own guard and against
// two indexers briefly racing during a misconfigured deploy. Inserts of new
// (source, sub) rows still succeed; the WHERE only gates the UPDATE path.
//
// first_ledger semantics (migration 0046):
//
//   - INSERT: first_ledger = lastLedger, the cursor's lower-bound coverage
//     anchor. For the live cursor (source='ledgerstream', empty sub-source)
//     the diagnostic density calc credits [first_ledger, last_ledger] as
//     covered.
//   - UPDATE: first_ledger is INTENTIONALLY PRESERVED via
//     `COALESCE(ingestion_cursors.first_ledger, EXCLUDED.first_ledger)`, so
//     restarts/resumes never move the anchor; it only moves backwards by an
//     explicit operator action (DELETE + re-insert). A NULL first_ledger
//     (pre-0046 row) is populated by the first UPDATE, so the coverage span
//     honestly starts at "we started tracking from here". The density
//     projection needs no NULL fallback: falling back to sourceGenesisLedger
//     would silently inflate density to 100% for sources with NULL live
//     cursors.
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

// AdvanceCursorFrom advances (source, sub) to newLast ONLY IF the row is
// still exactly what the caller read: a compare-and-swap for readers whose
// read→write gap is long enough for someone else to move the cursor. It
// reports whether the advance was applied; false with a nil error means "the
// cursor moved under you, abandon this commit and re-read".
//
// [Store.UpsertCursor]'s guard is monotonic-FORWARD against whatever the row
// holds NOW, wrong for the projector: its cycle reads the cursor, spends up
// to PerSourceTimeout scanning and sinking, then writes a position derived
// from that stale read. A `projector-replay` [Store.RewindCursor] landing in
// the gap writes a LOWER value, so the in-flight forward write would pass
// the guard and put the cursor back at tip: the replay prints success, its
// dirty window stays open, and nothing is re-projected.
//
// Comparing against the value READ makes the rewind win however they
// interleave: under READ COMMITTED an advance parked behind the rewind's row
// lock re-evaluates `last_ledger = $3` once unblocked.
//
// expected.Exists=false is the first-cycle seed: INSERT … ON CONFLICT DO
// NOTHING. first_ledger keeps UpsertCursor's semantics. newLast must be
// strictly above expected.LastLedger; moving backward is
// [Store.RewindCursor]'s job.
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

// RewindCursor moves an existing cursor BACKWARD to lastLedger — the
// deliberate-rewind path that UpsertCursor's monotonic-forward guard
// (WHERE EXCLUDED.last_ledger > last_ledger) intentionally
// refuses. `projector-replay` is the only production caller: rewinding
// the projector's per-source cursor is how historical re-projection
// works (ADR-0032 Phase 5).
//
// Without this method projector-replay would silently NO-OP: UpsertCursor
// with a lower ledger matches zero rows under the guard, the command
// prints success, and the projector stays at tip — a TRUNCATE + replay
// would write nothing.
//
// Errors if the cursor row doesn't exist — a rewind of a source that
// has never run is operator error, not a seed path (use UpsertCursor /
// the projector's own first cycle for that). Refuses to move FORWARD:
// fast-forwarding a cursor skips data and has its own deliberate SQL
// procedures; this method is single-purpose by design.
//
// Returns the ledger the row held when it was rewound, which can be above
// the caller's earlier read if a projector cycle committed in between:
// the re-walked range runs up to it. The FOR UPDATE CTE is what makes it
// the latest committed value under READ COMMITTED, not the snapshot's.
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

// ReapCursors deletes ingestion_cursors rows whose last_updated is strictly
// older than cutoff, skipping any row whose source is in `protected` and,
// when `source` is non-empty, any row outside that one source. Returns the
// number of rows deleted. The `stellarindex-ops reap-cursors` subcommand is
// the only caller; it previews first and passes the same arguments to the
// apply run, with [LiveCursorSources] as `protected`.
//
// `protected` is a parameter rather than read from [liveCursorSources] so
// the SQL guard is testable against a list the test controls; the caller's
// Go-side planner applies the same exclusion, and the two agreeing is the
// point of the second guard.
//
// The table needs reaping because every sharded one-shot job mints a row per
// shard that nothing removes. They record past work, not state anything
// reads (the live pipeline looks up its own (source, sub) key), so deleting
// one changes no ingest decision, only what every consumer that LISTS
// cursors sees.
//
// A cutoff-predicated DELETE (rather than one statement per previewed key)
// is exact because last_updated only moves FORWARD: UpsertCursor stamps
// now(), so no row can enter the `< cutoff` set between preview and apply. A
// row can only leave it, by being written to, which is the row an operator
// would want spared.
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
