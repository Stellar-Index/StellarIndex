-- 0064 up — `dex_volume_by_pair_1d` continuous aggregate.
--
-- Backs the DEX/AMM protocol-page bespoke analytics block
-- (internal/storage/timescale/protocol_bespoke.go::bespokeDEX). A direct
-- 90-day GROUP BY over the `trades` hypertable (313M rows at time of
-- writing) measured ~15.7s — too slow for a per-page request. This CAGG
-- rolls trades up to one row per (day-bucket, source, base_asset,
-- quote_asset) so the page's three queries (daily volume series, window
-- KPIs, top-pairs table) each read a few rows per pair instead of
-- scanning the raw hypertable.
--
-- Schema (one row per (source, base_asset, quote_asset, day-bucket)):
--   vol        = SUM(usd_volume)  — USD volume for trades that carried a
--                Phase-1 USD valuation. SUM ignores NULLs, so pools whose
--                quote never resolved to USD contribute 0 here (the page
--                Notes this caveat).
--   trades     = COUNT(*)         — total trades in the bucket.
--   base_vol   = SUM(base_amount) — base-leg turnover in base-asset units
--                (NOT USD; per-asset decimal scale).
--
-- Grain is 1 day: the page charts a daily series and sums over a 90-day
-- window, so a day bucket is the right resolution and keeps the row count
-- tiny (~one row per pair per day).
--
-- WITH NO DATA: the create does not backfill. After applying this
-- migration the operator MUST materialize the recent window once, e.g.
--   CALL refresh_continuous_aggregate('dex_volume_by_pair_1d',
--                                     now() - interval '120 days', now());
-- The add_continuous_aggregate_policy below keeps it current thereafter.

BEGIN;

CREATE MATERIALIZED VIEW dex_volume_by_pair_1d
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 day', ts)             AS bucket,
    source,
    base_asset,
    quote_asset,
    sum(usd_volume)                      AS vol,
    count(*)                             AS trades,
    sum(base_amount)                     AS base_vol
FROM trades
GROUP BY bucket, source, base_asset, quote_asset
WITH NO DATA;

CREATE INDEX dex_volume_by_pair_1d_source_idx
    ON dex_volume_by_pair_1d (source, bucket DESC);

CREATE INDEX dex_volume_by_pair_1d_pair_idx
    ON dex_volume_by_pair_1d (source, base_asset, quote_asset, bucket DESC);

-- Refresh recent + a 7-day window for late-arriving backfilled trades.
-- Hourly cadence: the day bucket only changes once a day, and 7 days of
-- buckets per (source, pair) is a small recompute.
SELECT add_continuous_aggregate_policy(
    'dex_volume_by_pair_1d',
    start_offset       => INTERVAL '7 days',
    end_offset         => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

COMMIT;
