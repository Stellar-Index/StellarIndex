package timescale

import (
	"context"
	"fmt"
)

// ProjectionDirtyWindow is one source's pending re-reconcile obligation
// (migration 0125): the ledger window a `stellarindex-ops projector-replay`
// rewound over that no completeness run has re-verified clean yet.
//
// It exists to close the carried-claim invalidation gap (2026-07-31): the
// daily compute-completeness driver reconciles each source only from its
// prior watermark to tip and CARRIES the prior clean projection claim for
// the older prefix (projectionClaim rule 3 / INV-5). That carry is sound
// only while the served tier below the watermark is immutable — and a
// projector-replay rewind rewrites exactly that region. Without a durable
// record of the rewind, the rewritten range stays certified by a claim
// whose evidence the replay just invalidated (how the 19,366 over-projected
// cctp rows of the 07-30 replay escaped the verifier).
type ProjectionDirtyWindow struct {
	Source string
	// From is the replay's rewind target (inclusive) — the first ledger
	// whose served rows the projector re-wrote.
	From uint32
	// To is the projector cursor at rewind time (inclusive). Ground above
	// it is new forward projection covered by the normal watermark-forward
	// reconcile; only the below-cursor range needs the forced re-check.
	To     uint32
	Reason string
}

// RecordProjectionDirtyWindow records (or widens) a source's dirty window.
// Called by projector-replay BEFORE it rewinds the cursor — that ordering
// is the fail-closed direction: a crash between record and rewind leaves a
// spurious window one clean verify clears, while the opposite order would
// leave a rewind with no record (the exact silent invalidation the table
// closes).
//
// The conflict arm WIDENS — LEAST(from), GREATEST(to) — rather than
// replacing: while a window is pending, a second replay can only grow the
// un-reverified obligation, never shrink it. Same monotonic discipline as
// completeness_target_floors' LEAST().
func (s *Store) RecordProjectionDirtyWindow(ctx context.Context, w ProjectionDirtyWindow) error {
	const q = `
        INSERT INTO projection_dirty_windows (source, from_ledger, to_ledger, reason)
        VALUES ($1, $2, $3, $4)
        ON CONFLICT (source) DO UPDATE SET
            from_ledger = LEAST(projection_dirty_windows.from_ledger, EXCLUDED.from_ledger),
            to_ledger   = GREATEST(projection_dirty_windows.to_ledger, EXCLUDED.to_ledger),
            reason      = EXCLUDED.reason,
            updated_at  = now()`
	if _, err := s.db.ExecContext(ctx, q,
		w.Source, int64(w.From), int64(w.To), w.Reason,
	); err != nil {
		return fmt.Errorf("timescale: RecordProjectionDirtyWindow (%s): %w", w.Source, err)
	}
	return nil
}

// ProjectionDirtyWindows returns every pending dirty window, keyed by
// source. A source absent from the map has no pending replay-rewind
// obligation. Callers must FAIL CLOSED on error (same discipline as
// CompletenessTargetFloors): a run blind to pending windows would carry
// projection claims over ranges a replay has rewritten.
func (s *Store) ProjectionDirtyWindows(ctx context.Context) (map[string]ProjectionDirtyWindow, error) {
	const q = `
        SELECT source, from_ledger, to_ledger, reason
        FROM projection_dirty_windows`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: ProjectionDirtyWindows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]ProjectionDirtyWindow)
	for rows.Next() {
		var w ProjectionDirtyWindow
		var from, to int64
		if err := rows.Scan(&w.Source, &from, &to, &w.Reason); err != nil {
			return nil, fmt.Errorf("timescale: ProjectionDirtyWindows scan: %w", err)
		}
		w.From = uint32(from) //nolint:gosec // ledger seq, bounded by the network head
		w.To = uint32(to)     //nolint:gosec // ledger seq, bounded by the network head
		out[w.Source] = w
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ProjectionDirtyWindows rows: %w", err)
	}
	return out, nil
}

// ClearProjectionDirtyWindow deletes a source's dirty window, but ONLY if
// the stored row is still within the [from, to] range the caller verified —
// the guard makes the clear race-safe against a concurrent replay: if
// another replay WIDENED the window between this run's read and its clear,
// the widened row no longer satisfies the bounds and survives, so the next
// run still sees the new obligation. Clearing unconditionally would let a
// clean verdict over the OLD window erase evidence of the NEW rewind.
func (s *Store) ClearProjectionDirtyWindow(ctx context.Context, source string, from, to uint32) error {
	const q = `
        DELETE FROM projection_dirty_windows
        WHERE source = $1 AND from_ledger >= $2 AND to_ledger <= $3`
	if _, err := s.db.ExecContext(ctx, q, source, int64(from), int64(to)); err != nil {
		return fmt.Errorf("timescale: ClearProjectionDirtyWindow (%s): %w", source, err)
	}
	return nil
}
