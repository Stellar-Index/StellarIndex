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
	// UpdatedAt is the row's last-write time, carried so a clear can
	// prove it is deleting the SAME row it read. See
	// [Store.ClearProjectionDirtyWindow].
	UpdatedAt time.Time
}

// Reason is PROVENANCE, not prose. Two tools write this table and they are
// not interchangeable to every reader:
//
//   - `projector-replay` RE-WINDS the live projector cursor, so the lag its
//     window covers is an INTENDED lag (issue #325).
//   - `projected-rebuild -write` never touches the live cursor, and its
//     recorded range may legally sit AT it (`-to` defaults to the live
//     cursor) or ABOVE it (`-allow-live-overlap` bypasses the one-writer
//     guard entirely — exercised on r1 2026-07-27).
//
// The constructors and the predicate below are the ONE place the format
// lives, so a reader can tell the two apart without matching a free-form
// string in three packages. The prefixes reproduce byte-for-byte what the
// shipped binaries have written since migration 0125, so rows ALREADY in
// the table classify correctly (pinned by
// TestProjectionDirtyWindowReasonIsStableAcrossReleases).
const (
	reasonProjectorReplayPrefix  = "projector-replay rewind "
	reasonProjectedRebuildPrefix = "projected-rebuild -write "
)

// ProjectorReplayReason is the Reason `stellarindex-ops projector-replay`
// stamps on the window it records before rewinding a source's cursor from
// fromCursor (its pre-rewind position, which becomes the window's To) to
// toTarget (the rewind target, which becomes its From).
func ProjectorReplayReason(fromCursor, toTarget uint32) string {
	return fmt.Sprintf("%s%d -> %d", reasonProjectorReplayPrefix, fromCursor, toTarget)
}

// ProjectedRebuildReason is the Reason `stellarindex-ops projected-rebuild
// -write` stamps on the window covering the range it is about to rewrite.
func ProjectedRebuildReason(from, to uint32) string {
	return fmt.Sprintf("%s[%d,%d]", reasonProjectedRebuildPrefix, from, to)
}

// IsProjectorReplay reports whether this window was recorded by
// `projector-replay` — the only writer that rewinds the live projector
// cursor, and therefore the only one whose window can EXPLAIN lag.
//
// Read by the projector's replay-window watcher (issue #325): a
// `projected-rebuild` window must never raise
// stellarindex_projector_replay_window_active, because its range routinely
// covers the live cursor's own position and would then hold the lag
// ticket suppressed while the source is HELD — exactly the state the
// ticket exists to catch.
//
// Unknown/empty reasons answer false: the flag's only power is to SUPPRESS
// an alert, so anything unrecognised must leave it armed.
func (w ProjectionDirtyWindow) IsProjectorReplay() bool {
	return strings.HasPrefix(w.Reason, reasonProjectorReplayPrefix)
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

// clearProjectionDirtyWindowQuery is package-level so its predicate can
// be asserted without a database — the guard it carries is the whole
// point of [Store.ClearProjectionDirtyWindow].
const clearProjectionDirtyWindowQuery = `
        DELETE FROM projection_dirty_windows
        WHERE source = $1 AND from_ledger >= $2 AND to_ledger <= $3
          AND updated_at = $4`

// ClearProjectionDirtyWindow deletes a source's dirty window, but ONLY if
// the stored row is still within the [from, to] range the caller verified —
// the guard makes the clear race-safe against a concurrent replay: if
// another replay WIDENED the window between this run's read and its clear,
// the widened row no longer satisfies the bounds and survives, so the next
// run still sees the new obligation. Clearing unconditionally would let a
// clean verdict over the OLD window erase evidence of the NEW rewind.
func (s *Store) ClearProjectionDirtyWindow(ctx context.Context, source string, from, to uint32, updatedAt time.Time) error {
	// `updated_at = $4` is the load-bearing conjunct, not the bounds.
	//
	// The bounds alone catch a WIDENED window — a concurrent replay that
	// grew the range leaves a row outside [from,to], which survives. They
	// do NOT catch a SUBSET RE-RECORD: a replay that re-records the same
	// or a narrower range leaves both bounds satisfying the predicate, so
	// the delete removes evidence of a rewind this run never verified,
	// and the next verdict carries a clean claim over it (wave-D CV-6).
	//
	// Comparing the row's last-write time makes this optimistic
	// concurrency: the clear succeeds only if the row is byte-for-byte
	// the one whose obligation this run actually discharged. Any
	// re-record — wider, narrower or identical — bumps updated_at and the
	// delete matches nothing, leaving the window pending for the next
	// run. Fail-closed, costing at most one extra reconcile.
	if _, err := s.db.ExecContext(ctx, clearProjectionDirtyWindowQuery, source, int64(from), int64(to), updatedAt); err != nil {
		return fmt.Errorf("timescale: ClearProjectionDirtyWindow (%s): %w", source, err)
	}
	return nil
}
