-- 0116 down — drop the durable per-target projection floor.
--
-- Destructive in a way worth stating plainly: this table is the ONLY
-- record of how far back each target was ever verified. Dropping it does
-- not just disable the check, it erases the history the check is built
-- on. A later re-apply starts from whatever MIN(ledger) happens to be at
-- that moment — so if rows were lost while this migration was reverted,
-- the new floor silently adopts the post-loss MIN and the loss becomes
-- permanently invisible. That is precisely the failure mode 0116 exists
-- to close.
--
-- The post-0116 binary reads this table on every projection reconcile, so
-- reverting must be paired with a rollback to a pre-0116 binary
-- (migrations/README.md rule 9's two-release dance). Reverting the
-- migration alone leaves the binary querying a table that no longer
-- exists.
--
-- If the goal is only to silence the new failure mode rather than to roll
-- back, do NOT run this: leave the table in place and address the reason
-- a target's MIN(ledger) rose above its floor.

BEGIN;

DROP TABLE IF EXISTS completeness_target_floors;

COMMIT;
