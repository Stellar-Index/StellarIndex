package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CompletenessSnapshot is one source's completeness verdict.
type CompletenessSnapshot struct {
	Source      string
	Genesis     uint32
	Tip         uint32
	Watermark   uint32
	CoveragePct float64
	Complete    bool
	// LakeComplete is the ADR-0033/ADR-0034 two-axis verdict's lake (archive) axis: substrate ∧ recognition only,
	// genesis-to-tip, decoupled from the retention-scoped projection reconcile that additionally gates Complete (the
	// served/combined axis).
	LakeComplete bool
	FirstProblem uint32 // 0 = none
	// FoundProblem is write-only (no column): this run found a failure FirstProblem cannot localise,
	// since the ClickHouse projection reconcile is an aggregate whose nonzero delta names no
	// ledger. Never set for a claim that was merely not evaluated.
	FoundProblem bool
	// ProjectionVerifiedFrom is the PROJECTION axis's floor: the source's genesis, or for a declared served window the minimum
	// served row over its targets. ProjectionOK claims nothing below it (Genesis is the LAKE floor, routinely far lower). 0
	// means not recorded; never read 0 as a floor.
	ProjectionVerifiedFrom uint32
	// ProjectionReconciledFrom is the lowest ledger THIS run's projection
	// reconcile covered (migration 0201); [ProjectionVerifiedFrom,
	// ProjectionReconciledFrom) was carried. 0 = not recorded.
	ProjectionReconciledFrom uint32
	// ProjectionEvidencedAt is when one run last reconciled the whole
	// served range cleanly — the oldest evidence behind ProjectionOK, which
	// a carry preserves and ComputedAt does not. Zero = none on record.
	ProjectionEvidencedAt time.Time
	// ProjectionEvidencedNow is write-only (no column): this run reconciled the whole served range cleanly, so the write stamps
	// projection_evidenced_at with the same now() as computed_at and ignores ProjectionEvidencedAt.
	ProjectionEvidencedNow bool
	SubstrateOK            bool
	RecognitionOK          bool
	ProjectionOK           bool
	Detail                 string
	ComputedAt             time.Time
}

// upsertCompletenessSnapshotQuery is the verdict write, shared so [Store.UpsertCompletenessSnapshot]
// and [Store.PublishCompletenessVerdict] run the byte-identical statement and never-regress guard.
const upsertCompletenessSnapshotQuery = `
        INSERT INTO completeness_snapshots (
            source, genesis_ledger, tip_ledger, watermark_ledger,
            coverage_pct, complete, lake_complete, first_problem_ledger,
            projection_verified_from,
            substrate_ok, recognition_ok, projection_ok, detail, computed_at,
            projection_reconciled_from, projection_evidenced_at
        ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13, now(),
            $15, CASE WHEN $16::boolean THEN now() ELSE $17::timestamptz END)
        ON CONFLICT (source) DO UPDATE SET
            genesis_ledger           = EXCLUDED.genesis_ledger,
            -- tip_ledger is network head and monotonic: GREATEST keeps a
            -- problem-arm write (below) from lowering it just because a
            -- regressive-window run also found a problem (CS-083).
            tip_ledger               = GREATEST(EXCLUDED.tip_ledger, completeness_snapshots.tip_ledger),
            watermark_ledger         = EXCLUDED.watermark_ledger,
            coverage_pct             = EXCLUDED.coverage_pct,
            complete                 = EXCLUDED.complete,
            lake_complete            = EXCLUDED.lake_complete,
            first_problem_ledger     = EXCLUDED.first_problem_ledger,
            projection_verified_from = EXCLUDED.projection_verified_from,
            substrate_ok             = EXCLUDED.substrate_ok,
            recognition_ok           = EXCLUDED.recognition_ok,
            projection_ok            = EXCLUDED.projection_ok,
            detail                   = EXCLUDED.detail,
            computed_at              = now(),
            projection_reconciled_from = EXCLUDED.projection_reconciled_from,
            projection_evidenced_at  = EXCLUDED.projection_evidenced_at
        -- CS-083: never let a regressive-window run (a smaller -to, or a
        -- mid-walk stall) overwrite a more-advanced verdict — that's how a
        -- source read complete=true pinned at a STALE tip. Apply the update
        -- only when this run advanced (or held) the tip, OR it found a
        -- problem (a newly-discovered problem must always be recorded, even
        -- if it lowers the watermark). The tip is monotonic (network head
        -- only grows), so a smaller tip means a stale/partial run — the
        -- problem arm still records the problem but tip_ledger itself is
        -- floored at GREATEST above, never regressed. $14 is the problem
        -- arm, computed by CompletenessSnapshot.foundProblem.
        WHERE EXCLUDED.tip_ledger >= completeness_snapshots.tip_ledger
           OR $14::boolean`

// snapshotExecer is the *sql.DB / *sql.Tx slice the verdict write needs, so it runs standalone or inside
// [Store.PublishCompletenessVerdict].
type snapshotExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// foundProblem is the never-regress guard's problem arm: this run's checks found a failure; an unevaluated claim does not qualify.
func (snap CompletenessSnapshot) foundProblem() bool {
	return snap.FirstProblem > 0 || snap.FoundProblem
}

// execCompletenessSnapshot reports whether the verdict was APPLIED: a regressive run matches zero rows yet the driver reports success.
func execCompletenessSnapshot(ctx context.Context, ex snapshotExecer, snap CompletenessSnapshot) (bool, error) {
	res, err := ex.ExecContext(ctx, upsertCompletenessSnapshotQuery,
		snap.Source, int64(snap.Genesis), int64(snap.Tip), int64(snap.Watermark),
		snap.CoveragePct, snap.Complete, snap.LakeComplete, int64(snap.FirstProblem),
		int64(snap.ProjectionVerifiedFrom),
		snap.SubstrateOK, snap.RecognitionOK, snap.ProjectionOK, snap.Detail,
		snap.foundProblem(),
		int64(snap.ProjectionReconciledFrom), snap.ProjectionEvidencedNow,
		sql.NullTime{Time: snap.ProjectionEvidencedAt, Valid: !snap.ProjectionEvidencedAt.IsZero()},
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

// UpsertCompletenessSnapshot writes a source's verdict without reporting whether the never-regress guard rejected it; callers clearing a dirty window on the verdict must use [Store.PublishCompletenessVerdict].
func (s *Store) UpsertCompletenessSnapshot(ctx context.Context, snap CompletenessSnapshot) error {
	if _, err := execCompletenessSnapshot(ctx, s.db, snap); err != nil {
		return fmt.Errorf("timescale: UpsertCompletenessSnapshot (%s): %w", snap.Source, err)
	}
	return nil
}

// DirtyWindowClear identifies the replay-rewind dirty window a verdict may clear: the exact row the run read (bounds AND updated_at), matching clearProjectionDirtyWindowQuery's optimistic predicate.
type DirtyWindowClear struct {
	From, To  uint32
	UpdatedAt time.Time
}

// VerdictPublication is what [Store.PublishCompletenessVerdict] did.
type VerdictPublication struct {
	// Applied is false when the never-regress guard rejected the write (this run's tip is below the stored tip and it found no
	// problem). The stored verdict is then UNCHANGED — whatever the run computed was not recorded.
	Applied bool
	// WindowCleared is true when the dirty window row was deleted. Always false when Applied is false; may also be false when
	// Applied is true because a concurrent replay re-recorded the window (see clearProjectionDirtyWindowQuery).
	WindowCleared bool
}

// PublishCompletenessVerdict writes a verdict and, only if that write was APPLIED, clears the dirty window it discharged in ONE transaction, because the never-regress guard rejects a lower-`-to` run with a nil error.
func (s *Store) PublishCompletenessVerdict(ctx context.Context, snap CompletenessSnapshot, clearWindow *DirtyWindowClear) (VerdictPublication, error) {
	var out VerdictPublication
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("timescale: PublishCompletenessVerdict (%s): begin: %w", snap.Source, err)
	}
	defer func() { _ = tx.Rollback() }()

	applied, err := execCompletenessSnapshot(ctx, tx, snap)
	if err != nil {
		return out, fmt.Errorf("timescale: PublishCompletenessVerdict (%s): upsert: %w", snap.Source, err)
	}
	var cleared bool
	if applied && clearWindow != nil {
		res, cerr := tx.ExecContext(ctx, clearProjectionDirtyWindowQuery,
			snap.Source, int64(clearWindow.From), int64(clearWindow.To), clearWindow.UpdatedAt)
		if cerr != nil {
			return out, fmt.Errorf("timescale: PublishCompletenessVerdict (%s): clear dirty window: %w", snap.Source, cerr)
		}
		n, cerr := res.RowsAffected()
		if cerr != nil {
			return out, fmt.Errorf("timescale: PublishCompletenessVerdict (%s): clear dirty window rows: %w", snap.Source, cerr)
		}
		cleared = n > 0
	}
	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("timescale: PublishCompletenessVerdict (%s): commit: %w", snap.Source, err)
	}
	out.Applied, out.WindowCleared = applied, cleared
	return out, nil
}

// ListCompletenessSnapshots returns every source's verdict, source-sorted.
func (s *Store) ListCompletenessSnapshots(ctx context.Context) ([]CompletenessSnapshot, error) {
	const q = `
        SELECT source, genesis_ledger, tip_ledger, watermark_ledger,
               coverage_pct, complete, lake_complete, first_problem_ledger,
               projection_verified_from,
               substrate_ok, recognition_ok, projection_ok, detail, computed_at,
               projection_reconciled_from, projection_evidenced_at
        FROM completeness_snapshots
        ORDER BY source`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListCompletenessSnapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []CompletenessSnapshot
	for rows.Next() {
		var (
			snap                        CompletenessSnapshot
			genesis, tip, wm, firstProb int64
			projFrom, reconciledFrom    int64
			evidencedAt                 sql.NullTime
		)
		if err := rows.Scan(
			&snap.Source, &genesis, &tip, &wm,
			&snap.CoveragePct, &snap.Complete, &snap.LakeComplete, &firstProb,
			&projFrom,
			&snap.SubstrateOK, &snap.RecognitionOK, &snap.ProjectionOK, &snap.Detail, &snap.ComputedAt,
			&reconciledFrom, &evidencedAt,
		); err != nil {
			return nil, fmt.Errorf("timescale: ListCompletenessSnapshots scan: %w", err)
		}
		snap.Genesis = uint32(genesis)
		snap.Tip = uint32(tip)
		snap.Watermark = uint32(wm)
		snap.FirstProblem = uint32(firstProb)
		snap.ProjectionVerifiedFrom = uint32(projFrom)
		snap.ProjectionReconciledFrom = uint32(reconciledFrom)
		if evidencedAt.Valid {
			snap.ProjectionEvidencedAt = evidencedAt.Time
		}
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListCompletenessSnapshots rows: %w", err)
	}
	return out, nil
}

// DeleteCompletenessSnapshots removes the verdict rows of sources compute-completeness classified as not applicable on a non-pubnet network; a nil or empty list is a no-op.
func (s *Store) DeleteCompletenessSnapshots(ctx context.Context, sources []string) (int64, error) {
	if len(sources) == 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM completeness_snapshots WHERE source = ANY($1)`,
		sources)
	if err != nil {
		return 0, fmt.Errorf("delete completeness snapshots: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
