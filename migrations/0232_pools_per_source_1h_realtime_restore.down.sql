-- 0232 down — return pools_per_source_1h to the materialized_only state 0187 left it in.

ALTER MATERIALIZED VIEW pools_per_source_1h SET (timescaledb.materialized_only = true);
