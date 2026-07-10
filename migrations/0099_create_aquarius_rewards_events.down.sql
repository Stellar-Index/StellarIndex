-- 0099 down — drop the Aquarius rewards-gauge event hypertable.
--
-- DROP TABLE removes the hypertable, its chunks, indexes and
-- compression policy in one statement. CASCADE is not needed —
-- nothing references this table (no CAGG, view, or FK).

BEGIN;

DROP TABLE IF EXISTS aquarius_rewards_events;

COMMIT;
