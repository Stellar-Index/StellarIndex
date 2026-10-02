-- 0199 up — record WHEN the projection claim was last proven, separately
-- from when it was last restated: add `projection_reconciled_from` and
-- `projection_evidenced_at` to completeness_snapshots.
--
-- computed_at is stamped now() on every applied write, whether the run
-- reconciled the whole served range or reconciled only the newest suffix
-- and CARRIED the prior clean claim over the rest (compute-completeness
-- projectionClaim rule 3). The deployed -pass audit carries by default:
-- a green source resumes at its prior watermark, so each nightly run
-- re-proves only the ledgers that closed since the last one, and the
-- carried prefix was last proven by whichever run last reconciled from
-- the served floor — possibly months earlier. computed_at cannot tell
-- the two apart, so the freshness gate on /v1/coverage read a stale
-- carried claim as fresh.
--
--   projection_reconciled_from — the lowest ledger THIS run's projection
--     reconcile covered. [projection_reconciled_from, watermark_ledger]
--     was proven at computed_at; [projection_verified_from,
--     projection_reconciled_from - 1] was carried. 0 = not recorded
--     (pre-0199 row, or projection not evaluated).
--   projection_evidenced_at — when one run last reconciled the WHOLE
--     served range cleanly: the age of the oldest evidence behind
--     projection_ok = true. A carry keeps the prior value; a fresh full
--     reconcile stamps now(). NULL = no evidence on record (no clean
--     projection claim, a pre-0199 row, or a carry from such a row).
--
-- Backfill: a row whose stored detail states that its run verified "the
-- full range the served tier holds" (projectionClaim rule 2's exact text)
-- with projection_ok still true was proven end to end at its computed_at,
-- so that IS its evidence time. Expect it to match ~0 rows on a deployed
-- host: there the nightly -pass carries, and a carry writes the carried
-- text, not rule 2's. It exists for a host whose latest verdicts came from
-- a full run. Every other row stays NULL — unknown — so after deploy the
-- -pass re-proves green sources from genesis, oldest/unknown evidence
-- first and at most three per night (-max-carry-age); /v1/coverage reads
-- stale until each has been re-proved. The sdex census is never re-proved
-- by the pass (its full reconcile outlasts the pass's deadline): it stays
-- NULL until the weekly compute-completeness-sdex.timer run, or a manual
-- `systemctl start compute-completeness-sdex.service`
-- (runbook completeness-incomplete.md).
--
-- Additive with DEFAULT 0 / NULL so the currently-deployed binary, whose
-- upsert does not list these columns, keeps working unmodified —
-- old-binary-safe per migrations/README.md rule 9. An old-binary write
-- leaves both columns untouched; that can only UNDER-state freshness
-- (a full reconcile it ran is not stamped), never over-state it.

BEGIN;

ALTER TABLE completeness_snapshots
    ADD COLUMN IF NOT EXISTS projection_reconciled_from bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS projection_evidenced_at timestamptz;

UPDATE completeness_snapshots
   SET projection_evidenced_at = computed_at
 WHERE projection_evidenced_at IS NULL
   AND projection_ok
   AND detail LIKE '%— the full range the served tier holds%';

COMMENT ON COLUMN completeness_snapshots.projection_reconciled_from IS
    'Lowest ledger THIS run''s projection reconcile covered: '
    '[projection_reconciled_from, watermark_ledger] was proven at '
    'computed_at, anything below it down to projection_verified_from was '
    'carried from the prior verdict. 0 = not recorded (pre-0199 row, or '
    'projection not evaluated).';

COMMENT ON COLUMN completeness_snapshots.projection_evidenced_at IS
    'When one run last reconciled the WHOLE served range cleanly — the age '
    'of the oldest evidence behind projection_ok. A carried claim keeps the '
    'prior value; computed_at is only when the claim was last restated. '
    'NULL = no evidence on record.';

COMMIT;
