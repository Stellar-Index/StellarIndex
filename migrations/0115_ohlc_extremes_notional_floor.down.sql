-- 0115 down — restore the UNFILTERED OHLC extremes of migration 0002.
--
-- Reverses 0115 up by recreating the seven price CAGGs with
-- `max/min/first/last(quote_amount / base_amount)` over every trade in
-- the bucket, i.e. exactly migration 0002's definitions, plus the
-- twap_1h / twap_1d dependents from migration 0081.
--
-- ⚠ This re-opens the B11-F1 dust-wick defect: a single 2-stroop
-- path-payment remainder can again set the served high or low for a
-- whole bar (production: XLM/USD 2026-07-17 06:00 served low 0.1333
-- against a real 0.1822). Roll back only to unblock an incident, and
-- prefer a NEW forward migration to any lasting change.
--
-- ⚠ Like the up, this leaves the CAGGs EMPTY (WITH NO DATA) — a full
-- operator re-materialization is required afterwards; see the up
-- migration's operator section for the refresh sequence.
--
-- RETENTION IS DELIBERATELY NOT RE-ADDED on prices_1m / prices_15m:
-- migration 0031 removed it and its own `down` is an intentional no-op
-- (ADR-0034 — the served history is kept forever). Restoring 0002's
-- retention policies here would resurrect the data-loss drift 0031
-- exists to prevent, so this `down` restores the 0002 *definitions*, not
-- the 0002 retention.

BEGIN;

CREATE TEMP TABLE _b11f1_prev_matonly ON COMMIT DROP AS
SELECT view_name, materialized_only
  FROM timescaledb_information.continuous_aggregates
 WHERE view_schema = 'public'
   AND view_name IN ('prices_1m', 'prices_15m', 'prices_1h', 'prices_4h',
                     'prices_1d', 'prices_1w', 'prices_1mo',
                     'twap_1h', 'twap_1d');

DROP MATERIALIZED VIEW IF EXISTS twap_1d;
DROP MATERIALIZED VIEW IF EXISTS twap_1h;
DROP MATERIALIZED VIEW IF EXISTS prices_1mo;
DROP MATERIALIZED VIEW IF EXISTS prices_1w;
DROP MATERIALIZED VIEW IF EXISTS prices_1d;
DROP MATERIALIZED VIEW IF EXISTS prices_4h;
DROP MATERIALIZED VIEW IF EXISTS prices_1h;
DROP MATERIALIZED VIEW IF EXISTS prices_15m;
DROP MATERIALIZED VIEW IF EXISTS prices_1m;

DO $do$
DECLARE
    g         record;
    view_name text;
BEGIN
    FOR g IN
        SELECT *
          FROM (VALUES
            -- grain, bucket expression,           start_offset, end_offset,   schedule
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
                first(quote_amount / base_amount, ts)                                AS first_price,
                last (quote_amount / base_amount, ts)                                AS last_price,
                max  (quote_amount / base_amount)                                    AS high_price,
                min  (quote_amount / base_amount)                                    AS low_price
            FROM trades
            GROUP BY bucket, base_asset, quote_asset
            WITH NO DATA
        $f$, view_name, g.bucket_expr);

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

-- twap_1h / twap_1d — verbatim migration 0081.
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
    sum(trade_count)                AS trade_count
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
    FOR v IN SELECT * FROM _b11f1_prev_matonly LOOP
        EXECUTE format(
            'ALTER MATERIALIZED VIEW %I SET (timescaledb.materialized_only = %L)',
            v.view_name, v.materialized_only);
    END LOOP;
END
$do$;

COMMIT;
