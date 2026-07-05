-- 0081 up — `twap_1h` + `twap_1d` continuous aggregates (TWAP for
-- /v1/chart?price_type=twap).
--
-- WHY A NEW CAGG (the prices_* views already carry a `twap` column):
-- migration 0002's `twap` column is `avg(quote_amount / base_amount)`
-- over the trades in the bucket — that is a TRADE-COUNT mean (each
-- trade weighted equally), NOT a time-weighted mean. A minute with
-- 1000 prints dominates a minute with 1 print. That is exactly the
-- weighting a TWAP must NOT have.
--
-- METHODOLOGY — time-weighted at 1-minute resolution:
--   twap_1h.twap = avg(prices_1m.twap)   over the ≤60 minute buckets
--   twap_1d.twap = avg(prices_1m.twap)   over the ≤1440 minute buckets
-- Each 1-minute sub-bucket contributes ONE equal-duration observation
-- (its minute-mean price) to the parent average, so every minute of
-- elapsed time counts the same regardless of how many trades printed
-- in it. This is the CAGG-feasible form of the local time-weighted
-- compute in internal/aggregate/twap.go (Σ price·Δt / Σ Δt with a
-- price active until the next trade): sampling the mean price each
-- minute and averaging the samples is the discrete time-weighted mean.
-- Proper LOCF Δt-weighting inside a bucket needs window functions
-- (LAG/LEAD) or the TimescaleDB Toolkit `time_weight` aggregate —
-- neither is available in a plain continuous aggregate on the
-- `timescale/timescaledb` image — so minute-resolution sampling is the
-- correct, portable approximation. Minutes with no trades are simply
-- not sampled (no LOCF carry-forward), matching the "closed buckets we
-- actually observed" contract the rest of the price surface uses.
--
-- HIERARCHICAL CAGG (CAGG-on-CAGG): both views read prices_1m (a
-- continuous aggregate) rather than the trades hypertable. prices_1m
-- lost its 30-day retention in migration 0031, so it is now indefinite
-- — these TWAP views inherit that indefinite reach (daily/hourly TWAP
-- back to the earliest materialized minute). 1 hour and 1 day are both
-- integer multiples of prices_1m's 1-minute bucket, as hierarchical
-- CAGGs require.
--
-- RETENTION: none — 1h/1d TWAP is indefinite, matching prices_1h /
-- prices_1d (migration 0002).
--
-- CLOSED-BUCKET (ADR-0015): as with prices_*, the read path
-- (timescale.TWAPPointsInRange) enforces closed-bucket via a
-- `bucket <= now() - <interval>` guard rather than a materialized_only
-- setting; the CAGG definition itself carries no materialized_only
-- override, exactly like the prices_* views.
--
-- WITH NO DATA: the create does NOT backfill. After applying this
-- migration the operator materializes the history once, e.g.
--   CALL refresh_continuous_aggregate('twap_1h', NULL, now());
--   CALL refresh_continuous_aggregate('twap_1d', NULL, now());
-- (run AFTER prices_1m itself is materialized). The policies below
-- keep both views current thereafter.
--
-- Column semantics per row:
--   bucket       = start of the aggregation window (timestamptz)
--   base_asset   = canonical base
--   quote_asset  = canonical quote
--   twap         = avg(prices_1m.twap) — minute-resolution time-weighted
--   volume       = Σ(prices_1m.volume)     — base-asset volume in window
--   volume_usd   = Σ(prices_1m.volume_usd) — USD-denominated volume
--   trade_count  = Σ(prices_1m.trade_count)

BEGIN;

-- 1-hour TWAP — hierarchical over prices_1m, RETAINED INDEFINITELY.
CREATE MATERIALIZED VIEW twap_1h
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 hour', bucket)   AS bucket,
    base_asset,
    quote_asset,
    avg(twap)                       AS twap,
    sum(volume)                     AS volume,
    sum(volume_usd)                 AS volume_usd,
    sum(trade_count)                AS trade_count
FROM prices_1m
GROUP BY time_bucket('1 hour', bucket), base_asset, quote_asset
WITH NO DATA;

CREATE INDEX twap_1h_pair_bucket_idx ON twap_1h (base_asset, quote_asset, bucket DESC);

-- Refresh every 15 min over the last 4 hours; 5-min grace so the
-- underlying prices_1m minute buckets (end_offset 30 s) are settled.
-- Mirrors prices_1h.
SELECT add_continuous_aggregate_policy(
    'twap_1h',
    start_offset      => INTERVAL '4 hours',
    end_offset        => INTERVAL '5 minutes',
    schedule_interval => INTERVAL '15 minutes'
);

-- No retention policy — indefinite by design.


-- 1-day TWAP — hierarchical over prices_1m, RETAINED INDEFINITELY.
CREATE MATERIALIZED VIEW twap_1d
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 day', bucket)    AS bucket,
    base_asset,
    quote_asset,
    avg(twap)                       AS twap,
    sum(volume)                     AS volume,
    sum(volume_usd)                 AS volume_usd,
    sum(trade_count)                AS trade_count
FROM prices_1m
GROUP BY time_bucket('1 day', bucket), base_asset, quote_asset
WITH NO DATA;

CREATE INDEX twap_1d_pair_bucket_idx ON twap_1d (base_asset, quote_asset, bucket DESC);

-- Refresh every 6 h over the last 7 days; 6-hour grace lets the day
-- settle. Mirrors prices_1d.
SELECT add_continuous_aggregate_policy(
    'twap_1d',
    start_offset      => INTERVAL '7 days',
    end_offset        => INTERVAL '6 hours',
    schedule_interval => INTERVAL '6 hours'
);

COMMIT;
