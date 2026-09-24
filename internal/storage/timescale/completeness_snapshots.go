package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CompletenessSnapshot is one source's completeness verdict
// (migration 0052, ADR-0033 Phase 6).
type CompletenessSnapshot struct {
	Source      string
	Genesis     uint32
	Tip         uint32
	Watermark   uint32
	CoveragePct float64
	Complete    bool
	// LakeComplete is the ADR-0033/ADR-0034 two-axis verdict's lake
	// (archive) axis: substrate ∧ recognition only, genesis-to-tip,
	// decoupled from the retention-scoped projection reconcile that
	// additionally gates Complete (the served/combined axis). See
	// notes/DECISION-genesis-complete-verdict-2026-07-16.md Option B.
	LakeComplete bool
	FirstProblem uint32 // 0 = none
	// FoundProblem is write-only (no column): this run's own check found a
	// failure that FirstProblem cannot localise — the ClickHouse projection
	// reconcile is an aggregate, so a nonzero delta, blind spots or floor
	// loss name no ledger. Never set for a claim that was merely not
	// evaluated. See [CompletenessSnapshot.foundProblem].
	FoundProblem bool
	// ProjectionVerifiedFrom is the PROJECTION axis's floor (migration
	// 0155): the lowest ledger the served tier holds any row at for this
	// source, computed by chops.projectionScopes as the minimum over the
	// source's targets. ProjectionOK is a claim about
	// [ProjectionVerifiedFrom, Watermark] and about nothing below it —
	// Genesis is the LAKE axis's floor and is routinely ten years lower.
	// 0 = not recorded (pre-0155 snapshot, or projection not evaluated);
	// never read 0 as a floor.
	ProjectionVerifiedFrom uint32
	SubstrateOK            bool
	RecognitionOK          bool
	ProjectionOK           bool
	Detail                 string
	ComputedAt             time.Time
}

// upsertCompletenessSnapshotQuery is the verdict write. Package-level so
// [Store.UpsertCompletenessSnapshot] and [Store.PublishCompletenessVerdict]
// run the byte-identical statement — the CS-083 guard it ends in is what
// makes "did this write land?" a question at all, and two copies of it
// would drift.
const upsertCompletenessSnapshotQuery = `
        INSERT INTO completeness_snapshots (
            source, genesis_ledger, tip_ledger, watermark_ledger,
            coverage_pct, complete, lake_complete, first_problem_ledger,
            projection_verified_from,
            substrate_ok, recognition_ok, projection_ok, detail, computed_at
        ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13, now())
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
            computed_at              = now()
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

// snapshotExecer is the slice of *sql.DB / *sql.Tx the verdict write
// needs, so the same statement runs standalone or inside
// [Store.PublishCompletenessVerdict]'s transaction.
type snapshotExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// foundProblem is the CS-083 guard's problem arm: this run's own checks
// found a failure, located (FirstProblem) or not (FoundProblem). A verdict
// that is false only because a claim was not evaluated does not qualify.
func (snap CompletenessSnapshot) foundProblem() bool {
	return snap.FirstProblem > 0 || snap.FoundProblem
}

// execCompletenessSnapshot runs the verdict write and reports whether it
// was APPLIED. The CS-083 guard makes a regressive run match zero rows,
// which the driver reports as success — so rows-affected is the only
// signal that a verdict was actually stored (finding F072).
func execCompletenessSnapshot(ctx context.Context, ex snapshotExecer, snap CompletenessSnapshot) (bool, error) {
	res, err := ex.ExecContext(ctx, upsertCompletenessSnapshotQuery,
		snap.Source, int64(snap.Genesis), int64(snap.Tip), int64(snap.Watermark),
		snap.CoveragePct, snap.Complete, snap.LakeComplete, int64(snap.FirstProblem),
		int64(snap.ProjectionVerifiedFrom),
		snap.SubstrateOK, snap.RecognitionOK, snap.ProjectionOK, snap.Detail,
		snap.foundProblem(),
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

// UpsertCompletenessSnapshot writes (or refreshes) a source's verdict.
//
// It does NOT report whether the CS-083 guard rejected the write. A caller
// whose next step depends on the verdict having been STORED — clearing a
// replay-rewind dirty window is the one that exists — must use
// [Store.PublishCompletenessVerdict] instead.
func (s *Store) UpsertCompletenessSnapshot(ctx context.Context, snap CompletenessSnapshot) error {
	if _, err := execCompletenessSnapshot(ctx, s.db, snap); err != nil {
		return fmt.Errorf("timescale: UpsertCompletenessSnapshot (%s): %w", snap.Source, err)
	}
	return nil
}

// DirtyWindowClear identifies the replay-rewind dirty window a verdict has
// earned the right to clear: the exact row the run read (bounds AND
// updated_at), matching [Store.ClearProjectionDirtyWindow]'s optimistic
// predicate.
type DirtyWindowClear struct {
	From, To  uint32
	UpdatedAt time.Time
}

// VerdictPublication is what [Store.PublishCompletenessVerdict] did.
type VerdictPublication struct {
	// Applied is false when the CS-083 never-regress guard rejected the
	// write (this run's tip is below the stored tip and it found no
	// problem). The stored verdict is then UNCHANGED — whatever the run
	// computed was not recorded.
	Applied bool
	// WindowCleared is true when the dirty window row was deleted. Always
	// false when Applied is false; may also be false when Applied is true
	// because a concurrent replay re-recorded the window (see
	// [Store.ClearProjectionDirtyWindow]).
	WindowCleared bool
}

// PublishCompletenessVerdict writes a source's verdict and — only if that
// write was APPLIED — clears the replay-rewind dirty window the verdict
// discharged, in ONE transaction (findings F072 / K013).
//
// The pair used to be two independent statements, and the first could not
// report that it did nothing. A run with a `-to` below the stored tip and
// no problem is rejected by the CS-083 guard with a nil error; the caller
// then deleted the window on the strength of a verdict that was never
// stored. The rewound range dropped out of every later reconcile floor
// while the STORED verdict still carried its pre-rewind clean claim over
// it — precisely the carried-claim invalidation the window exists to
// prevent.
//
// The window is the obligation and the stored verdict is its discharge, so
// the delete is gated on rows-affected and shares the verdict's
// transaction: a crash or error between the two rolls BOTH back (the
// window survives and the next run re-verifies — fail-closed), and a
// rejected verdict never reaches the delete at all. Under READ COMMITTED a
// verdict blocked behind a concurrent writer of the same source row
// re-evaluates the guard against the committed row once unblocked, so
// Applied reflects the row as it really is, not as it was when the run
// started.
//
// clearWindow == nil publishes the verdict alone.
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
               substrate_ok, recognition_ok, projection_ok, detail, computed_at
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
			projFrom                    int64
		)
		if err := rows.Scan(
			&snap.Source, &genesis, &tip, &wm,
			&snap.CoveragePct, &snap.Complete, &snap.LakeComplete, &firstProb,
			&projFrom,
			&snap.SubstrateOK, &snap.RecognitionOK, &snap.ProjectionOK, &snap.Detail, &snap.ComputedAt,
		); err != nil {
			return nil, fmt.Errorf("timescale: ListCompletenessSnapshots scan: %w", err)
		}
		snap.Genesis = uint32(genesis)
		snap.Tip = uint32(tip)
		snap.Watermark = uint32(wm)
		snap.FirstProblem = uint32(firstProb)
		snap.ProjectionVerifiedFrom = uint32(projFrom)
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListCompletenessSnapshots rows: %w", err)
	}
	return out, nil
}

// DeleteCompletenessSnapshots removes the verdict rows of the named
// sources. Used by compute-completeness on a non-pubnet network to clear
// rows written for pubnet-only sources before the catalogue was
// network-scoped (#483); the caller passes only catalogue names it has
// itself classified as not applicable, never arbitrary input. A nil or
// empty list is a no-op.
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
