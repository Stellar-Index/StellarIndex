package timescale

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ProjectionDirtyWindow is one source's pending re-reconcile obligation
// (migration 0125): the ledger window a `stellarindex-ops projector-replay`
// rewound over that no completeness run has re-verified clean yet.
//
// It closes the carried-claim invalidation gap: the daily compute-completeness driver reconciles
// each source only from its prior watermark to tip and CARRIES the prior clean projection claim
// for the older prefix (projectionClaim rule 3). That carry is sound only while the served tier
// below the watermark is immutable, and a projector-replay rewind rewrites exactly that region.
// Without a durable record of the rewind, the rewritten range stays certified by a claim whose
// evidence the replay just invalidated.
type ProjectionDirtyWindow struct {
	Source string
	// From is the rewind target (inclusive), the first ledger whose served rows were re-written.
	From uint32
	// To is the projector cursor at rewind time (inclusive), as returned by [Store.RewindCursor].
	// Ground above it is new forward projection the normal watermark reconcile covers.
	To     uint32
	Reason string
	// UpdatedAt lets a clear prove it deletes the SAME row it read (see
	// clearProjectionDirtyWindowQuery).
	UpdatedAt time.Time
}

// Reason is PROVENANCE, not prose. Five tools write this table and they are not interchangeable
// to every reader:
//
//   - `projector-replay` RE-WINDS the live projector cursor, so the lag its window covers is an
//     INTENDED lag.
//   - `projected-rebuild -write` never touches the live cursor; its range may sit AT it (`-to`
//     defaults to it) or ABOVE it (`-allow-live-overlap`).
//   - `ch-rebuild -record-dirty-window` (scripts/ops/ch-rebuild-projected.sh), when a clean-slate
//     window was DELETEd and its re-derive did not complete: the range is EMPTY until the
//     recovery run finishes.
//   - `ch-rebuild -write` rewrites the range from the lake, recorded before its first write; it
//     never touches the cursor.
//   - `backfill -write` rewrites NON-projected served rows, recorded before their first write;
//     they have no projector cursor. `backfill -source soroban-events` also stamps the full range
//     on every PROJECTED source with a cursor: those rows are not rewritten but stale until a
//     projector-replay re-projects them.
//
// The constructors and the predicate below are the ONE place the format lives. The prefixes
// reproduce byte-for-byte what released binaries have written since migration 0125 (pinned by
// TestProjectionDirtyWindowReasonIsStableAcrossReleases).
const (
	reasonProjectorReplayPrefix  = "projector-replay rewind "
	reasonProjectedRebuildPrefix = "projected-rebuild -write "
	reasonCHRebuildEmptiedPrefix = "ch-rebuild-projected emptied "
	reasonCHRebuildWritePrefix   = "ch-rebuild -write "
	reasonBackfillWritePrefix    = "backfill -write "
)

// ProjectorReplayReason is the Reason `projector-replay` stamps on its window: fromCursor (the
// pre-rewind position) becomes the window's To, toTarget its From.
func ProjectorReplayReason(fromCursor, toTarget uint32) string {
	return fmt.Sprintf("%s%d -> %d", reasonProjectorReplayPrefix, fromCursor, toTarget)
}

// ProjectedRebuildReason is the Reason `projected-rebuild -write` stamps on its range.
func ProjectedRebuildReason(from, to uint32) string {
	return fmt.Sprintf("%s[%d,%d]", reasonProjectedRebuildPrefix, from, to)
}

// CHRebuildEmptiedReason is the Reason `ch-rebuild -record-dirty-window` stamps on a window that
// scripts/ops/ch-rebuild-projected.sh DELETEd and could not yet re-derive. Distinct from
// ProjectedRebuildReason because the served tier is EMPTY over [from,to], not merely rewritten:
// until a verdict re-reconciles it, /v1/coverage would carry its prior clean claim over a hole.
func CHRebuildEmptiedReason(from, to uint32) string {
	return fmt.Sprintf("%s[%d,%d]", reasonCHRebuildEmptiedPrefix, from, to)
}

// CHRebuildWriteReason is the Reason `ch-rebuild -write` stamps on its range.
func CHRebuildWriteReason(from, to uint32) string {
	return fmt.Sprintf("%s[%d,%d]", reasonCHRebuildWritePrefix, from, to)
}

// BackfillWriteReason is the Reason `backfill -write` stamps on its range.
func BackfillWriteReason(from, to uint32) string {
	return fmt.Sprintf("%s[%d,%d]", reasonBackfillWritePrefix, from, to)
}

// IsProjectorReplay reports whether this window was recorded by
// `projector-replay`, the only writer that rewinds the live projector cursor and so the only one
// whose window can EXPLAIN lag.
//
// Read by the projector's replay-window watcher: a `projected-rebuild` window must never raise
// stellarindex_projector_replay_window_active, because its range routinely covers the live
// cursor's position and would suppress the lag ticket while the source is HELD, the very state
// the ticket exists to catch.
//
// Unknown/empty reasons answer false: the flag only SUPPRESSES an alert, so anything
// unrecognised must leave it armed.
func (w ProjectionDirtyWindow) IsProjectorReplay() bool {
	return strings.HasPrefix(w.Reason, reasonProjectorReplayPrefix)
}

// RecordProjectionDirtyWindow records (or widens) a source's dirty window. projector-replay calls
// it BEFORE rewinding: a crash between the two leaves a spurious window one clean verify clears,
// the opposite order a rewind with no record.
//
// The conflict arm WIDENS (LEAST(from), GREATEST(to)): a second replay can only grow the
// un-reverified obligation while one is pending, never shrink it.
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
// source; an absent source has no pending obligation. Callers must FAIL CLOSED on error (as with
// CompletenessTargetFloors): a run blind to pending windows would carry projection claims over
// ranges a replay has rewritten.
func (s *Store) ProjectionDirtyWindows(ctx context.Context) (map[string]ProjectionDirtyWindow, error) {
	const q = `
        SELECT source, from_ledger, to_ledger, reason, updated_at
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
		if err := rows.Scan(&w.Source, &from, &to, &w.Reason, &w.UpdatedAt); err != nil {
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

// clearProjectionDirtyWindowQuery deletes a source's dirty window, but ONLY if the stored row is
// still the one the caller verified; it runs inside [Store.PublishCompletenessVerdict]'s
// transaction. Package-level so the predicate can be asserted without a database.
//
// The bounds catch a WIDENED window (a concurrent replay that grew the range survives) but not a
// same-or-narrower re-record, so `updated_at = $4` is the load-bearing conjunct: any re-record
// bumps updated_at and the delete matches nothing, leaving the window pending.
const clearProjectionDirtyWindowQuery = `
        DELETE FROM projection_dirty_windows
        WHERE source = $1 AND from_ledger >= $2 AND to_ledger <= $3
          AND updated_at = $4`
