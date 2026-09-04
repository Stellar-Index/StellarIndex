-- 0155 down — drop `projection_verified_from`.
--
-- Not lossy: the column is derived, re-computed from the served tier on
-- every compute-completeness run, so a re-apply repopulates it on the
-- next pass. Unlike 0116's completeness_target_floors (which is the ONLY
-- record of how far back a target was ever verified), nothing here is
-- historical evidence.
--
-- The post-0155 binary selects this column on every verdict read, so
-- reverting must be paired with a rollback to a pre-0155 binary
-- (migrations/README.md rule 9's two-release dance).

BEGIN;

ALTER TABLE completeness_snapshots
    DROP COLUMN IF EXISTS projection_verified_from;

COMMIT;
