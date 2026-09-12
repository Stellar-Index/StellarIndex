-- Revert 0159: drop the SEP-1 retry ladder columns, its index, and restore
-- the 0023 comment on sep1_resolved_at.
--
-- ASYMMETRY, stated: the failure streaks accumulated while 0159 was applied
-- are discarded, and a re-up re-seeds only the coarse "attempted, never
-- produced a payload → 1" fact. Every deferral is also lifted, so the first
-- run after a down puts the whole dead-domain population back in the queue
-- at once — which is exactly the pre-0159 behaviour it is reverting to.

BEGIN;

DROP INDEX IF EXISTS issuers_sep1_refresh_queue_idx;

ALTER TABLE issuers DROP CONSTRAINT IF EXISTS issuers_sep1_consecutive_failures_check;

ALTER TABLE issuers
    DROP COLUMN IF EXISTS sep1_consecutive_failures,
    DROP COLUMN IF EXISTS sep1_next_attempt_after;

COMMENT ON COLUMN issuers.sep1_resolved_at IS NULL;

COMMIT;
