-- 0155 up — publish the projection floor as data: add
-- `projection_verified_from` to completeness_snapshots.
--
-- The projection axis has never been a genesis-to-tip claim for a
-- trade-emitting source: compute-completeness derives each target's
-- reconcile range from the served tier's OWN bottom edge
-- (internal/ops/chops/compute_completeness.go targetScope /
-- projectionScopes), and the per-source minimum of those — servedFrom —
-- is the lowest ledger the served tier holds anything at for that
-- source. On pubnet that is ledger ~61.6M for sdex and the five oracle
-- sources, while genesis_ledger for the same rows is 2.
--
-- That floor was computed on every run and then dropped on the floor.
-- It survived only inside the free-text `detail` string ("projection:
-- verified [64234754,64249915]; [61609957,64234753] carried from the
-- prior clean verdict"), which no machine consumer parses. A client
-- reading the TYPED fields — complete, coverage_pct, genesis_ledger —
-- got "verified complete from ledger 2", off by ten years.
--
-- Storing it makes the served axis's own extent readable next to the
-- claim it qualifies, the same way 0108 surfaced the lake axis that
-- `complete` had always silently mixed in. It is a projection-axis
-- field: watermark_ledger/coverage_pct stay the LAKE axis (0108) and
-- are untouched.
--
-- Relationship to 0116's completeness_target_floors, which carries a
-- column of the same name: that table is the DURABLE per-target floor
-- used to detect bottom-edge loss (a live MIN above the floor is loss,
-- not scope). This column is the LIVE per-source floor of the verdict
-- as computed, and is published. Same quantity, different granularity
-- and different job — hence the shared name.
--
-- 0 means "no projection floor recorded": a snapshot written before
-- this column existed, or a run whose projection axis was not evaluated
-- at all (an earlier claim already failed at genesis). It is NOT a
-- claim that the source projects from ledger 0, which is why readers
-- must treat 0 as absent rather than as a floor.
--
-- Additive with a DEFAULT so the currently-deployed binary (whose
-- upsert does not list this column) keeps working unmodified —
-- old-binary-safe per migrations/README.md rule 9.

BEGIN;

ALTER TABLE completeness_snapshots
    ADD COLUMN projection_verified_from bigint NOT NULL DEFAULT 0;

COMMENT ON COLUMN completeness_snapshots.projection_verified_from IS
    'PROJECTION-axis floor: the lowest ledger the served tier holds any '
    'row at for this source, i.e. the bottom of the range projection_ok '
    'is a claim about. Below it the served tier holds nothing, so '
    'complete/coverage_pct say nothing about it — genesis_ledger is the '
    'lake axis''s floor, not this one. 0 = not recorded (pre-0155 '
    'snapshot, or projection not evaluated); never read 0 as a floor.';

COMMIT;
