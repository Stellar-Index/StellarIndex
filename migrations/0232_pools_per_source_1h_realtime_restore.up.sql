-- 0232 up — switch pools_per_source_1h back to real-time aggregation.
--
-- 0187 recreated the view materialized_only = true and left restoring its
-- real-time tail (on since 0076) to an operator comment that no migration
-- or tool runs. A fresh install or a DR rebuild therefore stays
-- materialized-only: /v1/pools and /v1/markets?source= omit every trade of
-- the current hour while the code (timescale.realTimeCAGGs) treats the view
-- as real-time and the closed-bucket readyz check cannot see the drift.
--
-- The real-time half reads raw `trades` above the materialization
-- watermark, so flipping a view whose watermark lags far behind a populated
-- `trades` would scan every trade past it on each read. The ALTER runs only
-- when the watermark is within 6 hours of now (the refresh policy keeps it
-- there), or `trades` is empty (a fresh install, where the watermark tracks
-- the refresh policy from the start). Having rows is not enough: a view
-- refreshed once and then left stale still lags. Otherwise it raises a
-- NOTICE and the operator refreshes and then runs the same ALTER.
-- Re-running it on a view already real-time is a no-op.

SET LOCAL lock_timeout = '5s';

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM trades LIMIT 1)
       OR (SELECT _timescaledb_functions.to_timestamp(_timescaledb_functions.cagg_watermark(ca.mat_hypertable_id))
             FROM _timescaledb_catalog.continuous_agg ca
            WHERE ca.user_view_schema = 'public' AND ca.user_view_name = 'pools_per_source_1h')
          >= now() - interval '6 hours' THEN
        ALTER MATERIALIZED VIEW pools_per_source_1h SET (timescaledb.materialized_only = false);
    ELSE
        RAISE NOTICE '0232: pools_per_source_1h watermark is more than 6 hours behind a populated trades table; left materialized_only. Refresh it (0187 header), then run: ALTER MATERIALIZED VIEW pools_per_source_1h SET (timescaledb.materialized_only = false)';
    END IF;
END
$$;
