-- 0163 down — drop the per-window durable freeze ladders.
--
-- What this loses, stated plainly: `window_ladders` is the only durable
-- record of WHICH window of a pair owns which ADR-0019 ladder. Reverting
-- restores the pre-0163 behaviour in which the pair's single pair-level
-- ladder is whichever window wrote last, so after a Redis loss an ESCALATED
-- window can be rehydrated from a sibling's fresh ladder and resume
-- auto-unfreezing, and every window of the pair rehydrates the same freeze.
--
-- The freeze itself is NOT lost. The 0119 columns (hold_until,
-- extensions_used, escalated, corroborated) are kept in step as the
-- fail-closed summary of the per-window entries — furthest hold, highest
-- rung, escalated if any window is — so after this down a pre-0163 binary
-- rehydrates that summary onto every window: over-held, never dropped.
--
-- Export the per-window ladders first if anything is frozen:
--
--   SELECT asset_id, quote_id, frozen_at, hold_until, window_ladders
--     FROM freeze_events
--    WHERE recovered_at IS NULL;
--
-- The post-0163 aggregator WRITES this column on every lifecycle
-- transition, so reverting must be paired with a rollback to a pre-0163
-- binary (migrations/README.md rule 9). Reverting the migration alone
-- leaves the freeze writer UPDATE-ing a column that no longer exists; the
-- write is best-effort and counted
-- (stellarindex_anomaly_freeze_ladder_write_failures_total), so the
-- serving path keeps working, but NO durable ladder is recorded at all.

BEGIN;

ALTER TABLE freeze_events
    DROP COLUMN IF EXISTS window_ladders;

COMMIT;
