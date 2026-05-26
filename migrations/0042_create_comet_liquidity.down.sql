-- 0042 down — drop the comet_liquidity hypertable (#26).
--
-- DROP TABLE removes the hypertable, its chunks, indexes and
-- compression policy in one statement. CASCADE is not needed —
-- nothing references comet_liquidity (no CAGG, view, or FK).

BEGIN;

DROP TABLE IF EXISTS comet_liquidity;

COMMIT;
