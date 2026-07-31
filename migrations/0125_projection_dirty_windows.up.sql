-- 0124 up — remember the ledger window a projector-replay rewound over, so
-- the completeness verifier is forced to re-reconcile it (the 2026-07-31
-- carried-claim invalidation gap).
--
-- The daily compute-completeness driver reconciles each source only from
-- its prior watermark to tip and CARRIES the prior clean projection claim
-- for the older prefix (projectionClaim rule 3, INV-5). That carry is
-- sound only while the served tier below the watermark is IMMUTABLE. A
-- `stellarindex-ops projector-replay` rewind breaks that premise: it
-- rewrites served rows BELOW the watermark, and nothing forced the next
-- run to re-reconcile the rewritten range — the carried claim silently
-- kept certifying rows the replay had since changed. That is exactly how
-- the 19,366 over-projected cctp rows (the 07-30 replay's event_index-0
-- twins at 62.27M–63.55M) escaped the verifier while the soroswap twins —
-- caught by a full-range run — did not.
--
-- The fix is a per-source dirty window the replay records BEFORE it
-- rewinds the cursor: [from_ledger, to_ledger] = [rewind target, cursor at
-- rewind time] — the below-watermark range the projector is about to
-- rewrite. compute-completeness extends its projection reconcile floor to
-- cover any pending window (regardless of -from) and deletes the row only
-- after a CLEAN projection verdict whose scope covered it. A dirty verdict
-- keeps the window pending, so every subsequent run keeps re-checking the
-- rewound range until it verifies clean.
--
-- One row per source (PRIMARY KEY source), widened on conflict —
-- LEAST(from), GREATEST(to) — rather than one row per replay: the verifier
-- needs "the union of un-reverified rewound ground", not a replay audit
-- log (the journal already has that). Widening-only mirrors the
-- completeness_target_floors LEAST() discipline: a second replay while the
-- first window is pending can only grow the obligation, never shrink it.
--
-- Ordering contract (the fail-closed direction): the replay records the
-- window BEFORE rewinding the cursor. A crash between the two leaves a
-- spurious dirty window — harmless, one clean verify clears it. The
-- opposite order would leave a rewind with no record: the exact silent
-- invalidation this table exists to close.
--
-- Additive: a new table, no existing column touched, so an old binary
-- ignores it entirely (migrations/README.md rule 9).

BEGIN;

CREATE TABLE IF NOT EXISTS projection_dirty_windows (
    source      text        NOT NULL,
    from_ledger bigint      NOT NULL,
    to_ledger   bigint      NOT NULL,
    reason      text        NOT NULL DEFAULT '',
    recorded_at timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source)
);

COMMENT ON TABLE projection_dirty_windows IS
    'Per-source ledger window a projector-replay rewound over and no '
    'completeness run has re-reconciled clean yet (ADR-0033; the '
    '2026-07-31 carried-claim invalidation gap). Written by '
    'stellarindex-ops projector-replay BEFORE it rewinds the cursor; '
    'compute-completeness extends its projection reconcile floor to '
    'cover a pending window and deletes the row only on a clean '
    'projection verdict whose scope covered it. The upsert widens '
    '(LEAST(from), GREATEST(to)) so overlapping replays union their '
    'obligation.';

COMMENT ON COLUMN projection_dirty_windows.from_ledger IS
    'Inclusive lower bound: the replay''s rewind target — the first '
    'ledger whose served rows the projector re-wrote.';

COMMENT ON COLUMN projection_dirty_windows.to_ledger IS
    'Inclusive upper bound: the projector cursor at rewind time. Ground '
    'above it is new forward projection, covered by the normal '
    'watermark-forward reconcile; the dirty obligation is only the '
    'below-cursor range the carried claim would otherwise skip.';

COMMIT;
