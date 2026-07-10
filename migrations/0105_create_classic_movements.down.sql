-- 0105 down — drop the classic_movements hypertable.
--
-- DROP TABLE removes the hypertable, its chunks, indexes and
-- compression policy in one statement. CASCADE is not needed —
-- nothing references this table (no CAGG, view, or FK).

BEGIN;

DROP TABLE IF EXISTS classic_movements;

COMMIT;
