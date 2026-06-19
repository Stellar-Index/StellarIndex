-- 0068 down — drop the per-source hourly volume CAGG.
--
-- Removes the refresh policy first (TimescaleDB requires this before
-- dropping a continuous aggregate that has one attached), then the
-- materialized view. CASCADE handles any future code that joined the
-- CAGG (today only sourceVolumeHistory reads it, in a separate commit).

BEGIN;

SELECT remove_continuous_aggregate_policy('source_volume_1h', if_exists => true);

DROP MATERIALIZED VIEW IF EXISTS source_volume_1h CASCADE;

COMMIT;
