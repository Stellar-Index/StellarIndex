-- 0045 down — drop the Blend money-market hypertables (#25).
--
-- DROP TABLE removes each hypertable, its chunks, indexes and
-- compression settings in one statement. CASCADE is not needed —
-- nothing references these tables (the Blend money-market source
-- is the sole writer; no CAGG or view is built on top of them).

BEGIN;

DROP TABLE IF EXISTS blend_admin;
DROP TABLE IF EXISTS blend_emissions;
DROP TABLE IF EXISTS blend_positions;

COMMIT;
