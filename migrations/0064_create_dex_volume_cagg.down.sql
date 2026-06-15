-- 0064 down — drop the DEX volume CAGG.
--
-- Remove the refresh policy first (TimescaleDB requires this before
-- dropping a continuous aggregate that has a policy attached), then the
-- materialized view. CASCADE handles any view/index built on top.

BEGIN;

SELECT remove_continuous_aggregate_policy('dex_volume_by_pair_1d', if_exists => true);

DROP MATERIALIZED VIEW IF EXISTS dex_volume_by_pair_1d CASCADE;

COMMIT;
