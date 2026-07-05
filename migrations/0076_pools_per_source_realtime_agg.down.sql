-- 0076 down — revert pools_per_source_1h to materialized-only.
--
-- Restores TimescaleDB's default (no real-time tail). The current
-- in-progress hour goes invisible again until the refresh policy
-- materializes it — see 0076 up for why that's undesirable.

BEGIN;

ALTER MATERIALIZED VIEW pools_per_source_1h SET (timescaledb.materialized_only = true);

COMMIT;
