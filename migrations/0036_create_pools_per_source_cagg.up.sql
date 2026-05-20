-- 0036 up — per-source pools continuous aggregate (#25).
--
-- The durable backing for /v1/pools. Pre-#25 the handler's
-- buildPoolsQuery scanned the full trades hypertable for
-- `ts >= NOW() - INTERVAL '24 hours'` GROUP BY (source, base, quote)
-- — measured 8-30s on a populated 2.7B-row hypertable. #23 wrapped
-- it in stale-while-revalidate (sub-ms warm, ~8s cold first hit);
-- this CAGG eliminates the cold path too. Bucket grain is 1 hour:
-- coarse enough that the 24h-window query reads only ~24 rows per
-- (source, base, quote), fine enough that the post-CAGG vol_24h
-- recomputation has the right resolution.
--
-- Schema (one row per (source, base, quote, hour-bucket)):
--   sum_usd_priced       = SUM(usd_volume) — the trades that
--                          already had Phase-1 USD valuation. NULLs
--                          excluded by SUM by default.
--   sum_base_unpriced    = SUM(base_amount) FILTER usd_volume NULL
--                          — XLM-fallback input on the base side.
--   sum_quote_unpriced   = SUM(quote_amount) FILTER usd_volume NULL
--                          — XLM-fallback input on the quote side.
--   trade_count          = COUNT(*) — total trades in bucket.
--   bucket_last_price    = last(quote_amount/base_amount, ts) per
--                          hour; the handler post-aggregates with
--                          last() over 24 buckets for the overall
--                          most-recent price per pool.
--   bucket_last_ts       = last(ts, ts) — companion to
--                          bucket_last_price; handler picks the
--                          latest across the 24h window.
--
-- Refresh policy: every 5 minutes, covering the last 7 days
-- (matches the prices_1m cadence; over-refresh tolerates
-- late-arriving backfilled trades). Retention: none for now —
-- the CAGG rows are tiny (~handful per (source, base, quote)
-- per hour) and operators may want >24h windows later.

BEGIN;

CREATE MATERIALIZED VIEW pools_per_source_1h
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 hour', ts)                                   AS bucket,
    source,
    base_asset,
    quote_asset,
    sum(coalesce(usd_volume, 0)::numeric)                       AS sum_usd_priced,
    sum(base_amount)  FILTER (WHERE usd_volume IS NULL)         AS sum_base_unpriced,
    sum(quote_amount) FILTER (WHERE usd_volume IS NULL)         AS sum_quote_unpriced,
    count(*)                                                    AS trade_count,
    last(quote_amount / base_amount, ts)                        AS bucket_last_price,
    last(ts, ts)                                                AS bucket_last_ts
FROM trades
GROUP BY bucket, source, base_asset, quote_asset
WITH NO DATA;

CREATE INDEX pools_per_source_1h_lookup_idx
    ON pools_per_source_1h (source, base_asset, quote_asset, bucket DESC);

-- Refresh recent + a 7-day window for late-arriving backfilled
-- trades. 5-minute cadence matches prices_1m; the small bucket count
-- per cycle (~hundreds of (source, base, quote) × ~1 new bucket per
-- hour) keeps the refresh cheap.
SELECT add_continuous_aggregate_policy(
    'pools_per_source_1h',
    start_offset       => INTERVAL '7 days',
    end_offset         => INTERVAL '5 minutes',
    schedule_interval  => INTERVAL '5 minutes'
);

COMMIT;
