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
// cursor reaches" — rather than the pre-2026-05-28 fallback to
// sourceGenesisLedger which silently inflated density to 100%
// for sources whose live cursor stayed NULL. See migration 0046
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
// Monotonic-advance guard: the `WHERE` clause on DO UPDATE refuses
// to regress last_ledger. A lower-or-equal value is a silent no-op
// at the DB layer — protects against a caller that forgot its own
// guard (the orchestrator's cursorPersister has one too; this is
// defense-in-depth) and against two indexers briefly racing during
// a misconfigured deploy. Inserts of brand-new (source, sub) rows
// still succeed regardless; the WHERE only gates the UPDATE path.
//
// first_ledger semantics (migration 0046, 100% density mission):
//
//   - INSERT path: first_ledger = lastLedger. The first time this
//     (source, sub) is seen we capture the starting ledger as the
//     cursor's lower-bound coverage anchor. For the live cursor
//     (source='ledgerstream', sub-source empty) that's the first ledger this
//     region's live indexer ingested — the diagnostic density calc
//     credits the [first_ledger, last_ledger] band as covered.
//
//   - UPDATE path: first_ledger is INTENTIONALLY PRESERVED via
//     `COALESCE(ingestion_cursors.first_ledger, EXCLUDED.first_ledger)`.
//     This is two behaviours rolled into one expression:
//     (a) Non-NULL first_ledger (the steady-state case): COALESCE
//     returns the existing value → cursor advances without
//     moving its lower-bound coverage anchor. Live indexer
//     restarts/resumes do not stomp it; the anchor only ever
//     moves backwards by an explicit operator action
//     (DELETE + re-insert, not via this path).
//     (b) NULL first_ledger (pre-migration-0046 row that has
//     never been INSERT'd since the column was added): the
//     first UPDATE after deploy populates first_ledger with
//     EXCLUDED.first_ledger (the supplied lastLedger). The
//     live cursor's coverage span then becomes
//     [first-write-after-deploy, last_ledger] — honest about
//     "we started tracking from here", with no false claim
//     to genesis-onwards coverage. The diagnostic density
//     projection no longer needs a NULL fallback (it had
//     been falling back to sourceGenesisLedger and silently
//     inflating density to 100% for sources with NULL live
//     cursors — F-0020 density audit, 2026-05-28).
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

// RewindCursor moves an existing cursor BACKWARD to lastLedger — the
// deliberate-rewind path that UpsertCursor's monotonic-forward guard
// (WHERE EXCLUDED.last_ledger > last_ledger, F-0020) intentionally
// refuses. `projector-replay` is the only production caller: rewinding
// the projector's per-source cursor is how historical re-projection
// works (ADR-0032 Phase 5).
//
// Without this method projector-replay silently NO-OPed: it called
// UpsertCursor with a lower ledger, the guard matched zero rows, the
// command printed success, and the projector stayed at tip (found
// 2026-06-12 during the deliverable re-derives — the blend TRUNCATE +
// replay wrote nothing until this landed).
//
// Errors if the cursor row doesn't exist — a rewind of a source that
// has never run is operator error, not a seed path (use UpsertCursor /
// the projector's own first cycle for that). Refuses to move FORWARD:
// fast-forwarding a cursor skips data and has its own deliberate SQL
// procedures; this method is single-purpose by design.
func (s *Store) RewindCursor(ctx context.Context, source, sub string, lastLedger uint32) error {
	const q = `
        UPDATE ingestion_cursors
           SET last_ledger = $3, last_updated = now()
         WHERE source = $1 AND sub_source = $2 AND last_ledger > $3
    `
	res, err := s.db.ExecContext(ctx, q, source, sub, lastLedger)
	if err != nil {
		return fmt.Errorf("timescale: RewindCursor: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("timescale: RewindCursor rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("timescale: RewindCursor (%s,%s): no row rewound — cursor missing or already at/below ledger %d", source, sub, lastLedger)
	}
	return nil
}

// ReapCursors deletes ingestion_cursors rows whose last_updated is
// strictly older than cutoff, skipping any row whose source is in
// `protected` and — when `source` is non-empty — any row outside that
// one source. Returns the number of rows deleted. The `stellarindex-ops
// reap-cursors` subcommand is the only caller; it previews first and
// passes the same arguments to the apply run, with
// [LiveCursorSources] as `protected`.
//
// `protected` is a parameter rather than read from [liveCursorSources]
// here so the SQL guard is exercisable in a test against a list the
// test controls; the caller's Go-side planner applies the same
// exclusion, and the two agreeing is the point of the second guard.
//
// Why the table needs reaping at all: ingestion_cursors carries one
// permanent row per (source, sub_source), and every sharded one-shot
// job mints a row per shard that nothing ever removes — one abandoned
// SDEX backfill left 91 rows behind in May 2026, projected-rebuild
// 4,523. They are a record of past work, not state anything reads: the
// live pipeline looks up its own (source, sub) key, so a deleted
// historical row changes no ingest decision. What it does change is
// every consumer that LISTS cursors, which is why this exists rather
// than the rows being left to accumulate forever.
//
// A cutoff-predicated DELETE (rather than one statement per previewed
// key) is exact here because last_updated only ever moves FORWARD:
// UpsertCursor stamps now(), so no row can enter the `< cutoff` set
// between the preview and the apply. A row can only leave it — by being
// written to, which is precisely the row an operator would want spared.
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
