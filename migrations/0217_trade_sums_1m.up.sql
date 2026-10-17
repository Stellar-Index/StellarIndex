-- 0217 — trade_sums_1m: per-(source, pair) minute sums for the aggregator.
--
-- A liquid pair carries more trades in a 1h/24h window than one read may
-- return (MaxTradesPerWindow, 10,000), so the VWAP covered only the newest
-- slice. The aggregator now reads the capped leg's older minutes from this
-- view as one row per (source, minute): the per-source sums reproduce the
-- raw VWAP exactly, and min_price/max_price let it refuse to summarise a
-- minute whose prints spread wider than the outlier band.
--
-- Every sum is over priceable rows only (both legs > 0), matching the
-- aggregator's dropUnpriceable. No usd_volume column: the aggregator values
-- USD from quote_amount per source.
--
-- start_offset is 26 hours so a late insert into any bucket a 24h window can
-- read is re-materialised; the aggregator never summarises a window longer
-- than 25 hours. The policy fills the view from the first run, so no
-- operator refresh is needed. Retention (7 days) ships disabled, as 0169's.

CREATE MATERIALIZED VIEW trade_sums_1m
WITH (timescaledb.continuous, timescaledb.materialized_only = true) AS
SELECT
    time_bucket('1 minute', ts)                                          AS bucket,
    source,
    base_asset,
    quote_asset,
    sum(base_amount)  FILTER (WHERE base_amount > 0 AND quote_amount > 0) AS base_sum,
    sum(quote_amount) FILTER (WHERE base_amount > 0 AND quote_amount > 0) AS quote_sum,
    count(*)          FILTER (WHERE base_amount > 0 AND quote_amount > 0) AS trade_count,
    min(quote_amount / base_amount)
                      FILTER (WHERE base_amount > 0 AND quote_amount > 0) AS min_price,
    max(quote_amount / base_amount)
                      FILTER (WHERE base_amount > 0 AND quote_amount > 0) AS max_price
FROM trades
GROUP BY bucket, source, base_asset, quote_asset
WITH NO DATA;

CREATE INDEX trade_sums_1m_pair_bucket_idx ON trade_sums_1m (base_asset, quote_asset, bucket DESC);

SELECT add_continuous_aggregate_policy(
    'trade_sums_1m',
    start_offset      => INTERVAL '26 hours',
    end_offset        => INTERVAL '1 minute',
    schedule_interval => INTERVAL '1 minute'
);

SELECT add_retention_policy(
         'trade_sums_1m',
         drop_after    => INTERVAL '7 days',
         if_not_exists => true);

SELECT alter_job(job_id, scheduled => false)
  FROM timescaledb_information.jobs
 WHERE proc_name = 'policy_retention'
   AND hypertable_name = 'trade_sums_1m';

DO $$
DECLARE
  total  integer;
  active integer;
BEGIN
  SELECT count(*), count(*) FILTER (WHERE scheduled)
    INTO total, active
    FROM timescaledb_information.jobs
   WHERE proc_name = 'policy_retention'
     AND hypertable_name = 'trade_sums_1m';
  IF total <> 1 OR active <> 0 THEN
    RAISE EXCEPTION 'trade_sums_1m: expected 1 disabled retention policy, found % (% scheduled)', total, active;
  END IF;
END $$;
