-- 0089 down — drop the Aquarius liquidity + reserves hypertables.
--
-- DROP TABLE removes each hypertable, its chunks, indexes and
-- compression policy in one statement. CASCADE is not needed —
-- nothing references these tables (no CAGG, view, or FK).

BEGIN;

DROP TABLE IF EXISTS aquarius_liquidity;
DROP TABLE IF EXISTS aquarius_reserves;

COMMIT;
