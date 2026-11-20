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
-- watermark, so flipping an unrefreshed view over a populated `trades`
-- would scan the whole table on every read. The ALTER runs only when the
-- view already holds materialized rows, or `trades` is empty (a fresh
-- install, where the watermark tracks the refresh policy from the start).
-- Otherwise it raises a NOTICE and the operator runs 0187's refresh and
-- then the same ALTER. Re-running it on a view already real-time is a no-op.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pools_per_source_1h LIMIT 1)
       OR NOT EXISTS (SELECT 1 FROM trades LIMIT 1) THEN
        ALTER MATERIALIZED VIEW pools_per_source_1h SET (timescaledb.materialized_only = false);
    ELSE
        RAISE NOTICE '0232: pools_per_source_1h is unrefreshed over a populated trades table; left materialized_only. Refresh it (0187 header), then run: ALTER MATERIALIZED VIEW pools_per_source_1h SET (timescaledb.materialized_only = false)';
    END IF;
END
$$;
