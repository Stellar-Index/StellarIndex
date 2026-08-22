-- 0147 down — restore the ts-ordered (tie-UNSTABLE) first/last and the
-- legacy per-row VWAP of migration 0115, plus the 0126 twap dependents.
--
-- ⚠ This re-opens the determinism defect: bucket open/close ties resolve
-- by physical scan order again, so re-materializations can flip served
-- bars. Roll back only to unblock an incident; prefer a new forward
-- migration for any lasting change.
--
-- ⚠ Like the up, this leaves the CAGGs EMPTY (WITH NO DATA) — a full
-- operator re-materialization is required afterwards; see the up.

BEGIN;

CREATE TEMP TABLE _0147d_prev_matonly ON COMMIT DROP AS
SELECT view_name, materialized_only
  FROM timescaledb_information.continuous_aggregates
 WHERE view_schema = 'public'
   AND view_name IN ('prices_1m', 'prices_15m', 'prices_1h', 'prices_4h',
                     'prices_1d', 'prices_1w', 'prices_1mo',
                     'twap_1h', 'twap_1d');

DROP MATERIALIZED VIEW IF EXISTS twap_1d;  -- migration-compat:ok down-migration restore (0126 definition, identical columns)
DROP MATERIALIZED VIEW IF EXISTS twap_1h;  -- migration-compat:ok down-migration restore (0126 definition, identical columns)
DROP MATERIALIZED VIEW IF EXISTS prices_1mo; -- migration-compat:ok down-migration restore (0115 definition, identical columns)
DROP MATERIALIZED VIEW IF EXISTS prices_1w;  -- migration-compat:ok down-migration restore (0115 definition, identical columns)
DROP MATERIALIZED VIEW IF EXISTS prices_1d;  -- migration-compat:ok down-migration restore (0115 definition, identical columns)
DROP MATERIALIZED VIEW IF EXISTS prices_4h;  -- migration-compat:ok down-migration restore (0115 definition, identical columns)
DROP MATERIALIZED VIEW IF EXISTS prices_1h;  -- migration-compat:ok down-migration restore (0115 definition, identical columns)
DROP MATERIALIZED VIEW IF EXISTS prices_15m; -- migration-compat:ok down-migration restore (0115 definition, identical columns)
DROP MATERIALIZED VIEW IF EXISTS prices_1m;  -- migration-compat:ok down-migration restore (0115 definition, identical columns)

-- The seven price CAGGs exactly as migration 0115 created them.
DO $do$
DECLARE
    ohlc_extreme_min_usd_volume CONSTANT numeric := 0.01;

    dust_filter text := format('FILTER (WHERE usd_volume >= %L)',
                               ohlc_extreme_min_usd_volume);
    g           record;
    view_name   text;
BEGIN
    FOR g IN
        SELECT *
          FROM (VALUES
            ('1m',  $b$time_bucket('1 minute', ts)$b$,    '5 minutes',  '30 seconds', '30 seconds'),
            ('15m', $b$time_bucket('15 minutes', ts)$b$,  '1 hour',     '1 minute',   '5 minutes'),
            ('1h',  $b$time_bucket('1 hour', ts)$b$,      '4 hours',    '5 minutes',  '15 minutes'),
            ('4h',  $b$time_bucket('4 hours', ts)$b$,     '1 day',      '30 minutes', '1 hour'),
            ('1d',  $b$time_bucket('1 day', ts)$b$,       '7 days',     '6 hours',    '6 hours'),
            ('1w',  $b$time_bucket('1 week', ts)$b$,      '4 weeks',    '1 day',      '1 day'),
            ('1mo', $b$time_bucket('1 month', ts, 'UTC')$b$, '3 months', '1 day',     '1 day')
          ) AS t(grain, bucket_expr, start_offset, end_offset, schedule)
    LOOP
        view_name := 'prices_' || g.grain;

        EXECUTE format($f$
            CREATE MATERIALIZED VIEW %1$I
            WITH (timescaledb.continuous) AS
            SELECT
                %2$s                                                                 AS bucket,
                base_asset,
                quote_asset,
                sum( (quote_amount / base_amount) * base_amount ) / sum(base_amount) AS vwap,
                avg( quote_amount / base_amount )                                    AS twap,
                sum(base_amount)                                                     AS volume,
                sum(coalesce(usd_volume, 0))                                         AS volume_usd,
                count(*)                                                             AS trade_count,
                array_agg(DISTINCT source)                                           AS sources,
                COALESCE(first(quote_amount / base_amount, ts) %3$s,
                         first(quote_amount / base_amount, ts))                      AS first_price,
                COALESCE(last (quote_amount / base_amount, ts) %3$s,
                         last (quote_amount / base_amount, ts))                      AS last_price,
                COALESCE(max  (quote_amount / base_amount)      %3$s,
                         max  (quote_amount / base_amount))                          AS high_price,
                COALESCE(min  (quote_amount / base_amount)      %3$s,
                         min  (quote_amount / base_amount))                          AS low_price
            FROM trades
            GROUP BY bucket, base_asset, quote_asset
            WITH NO DATA
        $f$, view_name, g.bucket_expr, dust_filter);

        EXECUTE format(
            'CREATE INDEX %I ON %I (base_asset, quote_asset, bucket DESC)',
            view_name || '_pair_bucket_idx', view_name);

        PERFORM add_continuous_aggregate_policy(
            view_name::regclass,
            start_offset      => g.start_offset::interval,
            end_offset        => g.end_offset::interval,
            schedule_interval => g.schedule::interval);
    END LOOP;
END
$do$;

-- twap_1h / twap_1d — verbatim 0126 (sample_count preserved).
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
    FOR v IN SELECT * FROM _0147d_prev_matonly LOOP
        EXECUTE format(
            'ALTER MATERIALIZED VIEW %I SET (timescaledb.materialized_only = %L)',
            v.view_name, v.materialized_only);
    END LOOP;
END
$do$;

COMMIT;
