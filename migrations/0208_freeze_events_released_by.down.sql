-- 0208 down — drop freeze_events.released_by.
--
-- Loses the record of who closed each row since 0208; audit_log still holds
-- every operator unfreeze. Pair with a rollback to a pre-0208 binary: the
-- current MarkRecovered writes this column and would fail without it.

BEGIN;

ALTER TABLE freeze_events
    DROP COLUMN IF EXISTS released_by;

COMMIT;
