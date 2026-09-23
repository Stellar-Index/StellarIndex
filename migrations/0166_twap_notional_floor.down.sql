-- 0166 down — restore 0147's prices_1m and 0126's twap_1h / twap_1d
-- (unfloored twap, no notional_* columns), re-attaching 0156's
-- retention policy disarmed as the up does. prices_1m's refresh policy
-- keeps 0165's widened 15-minute start_offset — this migration only
-- undoes 0166's own notional-floor columns, not 0165's fix. Leaves the
-- three views EMPTY; re-materialise per the up migration's header.

BEGIN;

CREATE TEMP TABLE _0166d_prev_matonly ON COMMIT DROP AS
SELECT view_name, materialized_only
  FROM timescaledb_information.continuous_aggregates
 WHERE view_schema = 'public'
   AND view_name IN ('prices_1m', 'twap_1h', 'twap_1d');

DROP MATERIALIZED VIEW IF EXISTS twap_1d;   -- migration-compat:ok down-migration restore (0126 definition)
DROP MATERIALIZED VIEW IF EXISTS twap_1h;   -- migration-compat:ok down-migration restore (0126 definition)
DROP MATERIALIZED VIEW IF EXISTS prices_1m; -- migration-compat:ok down-migration restore (0147 definition)

CREATE MATERIALIZED VIEW prices_1m
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 minute', ts)                                          AS bucket,
    base_asset,
    quote_asset,
    sum(quote_amount) / sum(base_amount)                                 AS vwap,
    avg(quote_amount / base_amount)                                      AS twap,
    sum(base_amount)                                                     AS volume,
    sum(coalesce(usd_volume, 0))                                         AS volume_usd,
    count(*)                                                             AS trade_count,
    array_agg(DISTINCT source)                                           AS sources,
    COALESCE(first(quote_amount / base_amount,
                   lpad((extract(epoch FROM ts) * 1000000)::bigint::text, 19, '0')
                   || lpad(ledger::text, 10, '0') || tx_hash
                   || lpad(op_index::text, 10, '0') || source)
                   FILTER (WHERE usd_volume >= 0.01),
             first(quote_amount / base_amount,
                   lpad((extract(epoch FROM ts) * 1000000)::bigint::text, 19, '0')
                   || lpad(ledger::text, 10, '0') || tx_hash
                   || lpad(op_index::text, 10, '0') || source))      AS first_price,
    COALESCE(last(quote_amount / base_amount,
                  lpad((extract(epoch FROM ts) * 1000000)::bigint::text, 19, '0')
                  || lpad(ledger::text, 10, '0') || tx_hash
                  || lpad(op_index::text, 10, '0') || source)
                  FILTER (WHERE usd_volume >= 0.01),
             last(quote_amount / base_amount,
                  lpad((extract(epoch FROM ts) * 1000000)::bigint::text, 19, '0')
                  || lpad(ledger::text, 10, '0') || tx_hash
                  || lpad(op_index::text, 10, '0') || source))       AS last_price,
    COALESCE(max(quote_amount / base_amount) FILTER (WHERE usd_volume >= 0.01),
             max(quote_amount / base_amount))                            AS high_price,
    COALESCE(min(quote_amount / base_amount) FILTER (WHERE usd_volume >= 0.01),
             min(quote_amount / base_amount))                            AS low_price
FROM trades
GROUP BY bucket, base_asset, quote_asset
WITH NO DATA;

CREATE INDEX prices_1m_pair_bucket_idx ON prices_1m (base_asset, quote_asset, bucket DESC);

SELECT add_continuous_aggregate_policy(
    'prices_1m',
    start_offset      => INTERVAL '15 minutes',
    end_offset        => INTERVAL '30 seconds',
    schedule_interval => INTERVAL '30 seconds'
);

-- The up's re-attached policy went with the dropped view.
SELECT add_retention_policy(
         'prices_1m',
         drop_after    => INTERVAL '90 days',
         if_not_exists => true);

SELECT alter_job(job_id, scheduled => false)
  FROM timescaledb_information.jobs
 WHERE proc_name = 'policy_retention'
   AND hypertable_name = 'prices_1m';

DO $$
DECLARE
  total  integer;
  active integer;
BEGIN
  SELECT count(*), count(*) FILTER (WHERE scheduled)
    INTO total, active
    FROM timescaledb_information.jobs
   WHERE proc_name = 'policy_retention'
     AND hypertable_name = 'prices_1m';
  IF total <> 1 THEN
    RAISE EXCEPTION 'expected exactly 1 retention policy on prices_1m, found %', total;
  END IF;
  IF active <> 0 THEN
    RAISE EXCEPTION 'the prices_1m retention policy is still SCHEDULED after the disable step; '
                    'refusing to ship an armed destructive policy';
  END IF;
END $$;

CREATE MATERIALIZED VIEW twap_1h
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 hour', bucket)   AS bucket,
    base_asset,
    quote_asset,
    avg(twap)                       AS twap,
    sum(volume)                     AS volume,
    sum(volume_usd)                 AS volume_usd,
    sum(trade_count)                AS trade_count,
    count(*)                        AS sample_count
FROM prices_1m
GROUP BY time_bucket('1 hour', bucket), base_asset, quote_asset
WITH NO DATA;

CREATE INDEX twap_1h_pair_bucket_idx ON twap_1h (base_asset, quote_asset, bucket DESC);

SELECT add_continuous_aggregate_policy(
    'twap_1h',
    start_offset      => INTERVAL '4 hours',
    end_offset        => INTERVAL '5 minutes',
    schedule_interval => INTERVAL '15 minutes'
);

CREATE MATERIALIZED VIEW twap_1d
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 day', bucket)    AS bucket,
    base_asset,
    quote_asset,
    avg(twap)                       AS twap,
    sum(volume)                     AS volume,
    sum(volume_usd)                 AS volume_usd,
    sum(trade_count)                AS trade_count,
    count(*)                        AS sample_count
FROM prices_1m
GROUP BY time_bucket('1 day', bucket), base_asset, quote_asset
WITH NO DATA;

CREATE INDEX twap_1d_pair_bucket_idx ON twap_1d (base_asset, quote_asset, bucket DESC);

SELECT add_continuous_aggregate_policy(
    'twap_1d',
    start_offset      => INTERVAL '7 days',
    end_offset        => INTERVAL '6 hours',
    schedule_interval => INTERVAL '6 hours'
);

DO $do$
DECLARE
    v record;
BEGIN
    FOR v IN SELECT * FROM _0166d_prev_matonly LOOP
        EXECUTE format(
            'ALTER MATERIALIZED VIEW %I SET (timescaledb.materialized_only = %L)',
            v.view_name, v.materialized_only);
    END LOOP;
END
$do$;

COMMIT;
