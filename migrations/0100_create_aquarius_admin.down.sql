-- 0100 down — drop the Aquarius admin/governance event hypertable.
--
-- DROP TABLE removes the hypertable, its chunks, indexes and
-- compression policy in one statement. CASCADE is not needed —
-- nothing references this table (no CAGG, view, or FK).

BEGIN;

DROP TABLE IF EXISTS aquarius_admin;

COMMIT;
