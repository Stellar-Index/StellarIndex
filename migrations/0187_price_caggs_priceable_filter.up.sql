-- 0187 up — make every trades-derived price CAGG safe for zero-leg trades.
--
-- SDEX can settle a trade with one leg rounded to zero stroops. `trades`
-- still rejects those rows (0001's inline `> 0` CHECKs); a later
-- migration drops the CHECKs so they can be stored. Before that, every
-- view that divides quote_amount by base_amount has to stop doing so on
-- a zero leg: a base = 0 row fails the refresh with a division error, a
-- quote = 0 row prices at 0, and a one-leg row skews vwap. Today there
-- are no such rows, so every value this migration materialises equals
-- the one it replaces.
--
-- What changes (everything else is the current definition verbatim:
-- prices_1m and twap_1h/1d from 0166, prices_15m..prices_1mo from 0147,
-- pools_per_source_1h from 0036; refresh policies from 0165/0147/0126/
-- 0036; 0156's retention on prices_1m re-attached disarmed):
--
--   * every price expression is restricted to priceable rows
--     (base_amount > 0 AND quote_amount > 0): both branches of each
--     dust-floor COALESCE, vwap's numerator and denominator, the twap,
--     and pools_per_source_1h's bucket_last_price. A bucket with no
--     priceable row has NULL prices.
--   * prices_1m.notional_trade_count counts priceable rows only, and
--     twap_1h/1d's fallback sample_count counts minutes with a twap, so
--     both keep meaning "samples the twap averaged".
--   * new columns on the seven prices_* views: volume_quote =
--     sum(quote_amount) over ALL rows, and volume_priced =
--     sum(base_amount) over priceable rows (vwap * volume_priced is the
--     priced quote). `volume` stays sum(base_amount) over all rows.
--   * all ten views are created materialized_only = true, including
--     pools_per_source_1h (real-time since 0076): its real-time half
--     would scan raw `trades` until refreshed and divide at read time.
--     Real-time is switched back on only after it has been refreshed.
--
-- ─── ⚠ OPERATOR: this migration leaves all ten views EMPTY ────────────
-- WITH NO DATA, the 0115/0147/0166 pattern. Recent first, prices_1m
-- BEFORE the TWAP views, every refresh WINDOWED and forced:
--
--   CALL refresh_continuous_aggregate('prices_1m', now() - INTERVAL '7 days', now(), force => true);
--   CALL refresh_continuous_aggregate('prices_15m', now() - INTERVAL '7 days', now(), force => true);
--   CALL refresh_continuous_aggregate('prices_1h', now() - INTERVAL '7 days', now(), force => true);
--   CALL refresh_continuous_aggregate('prices_4h', now() - INTERVAL '7 days', now(), force => true);
--   CALL refresh_continuous_aggregate('prices_1d', now() - INTERVAL '7 days', now(), force => true);
--   CALL refresh_continuous_aggregate('prices_1w', now() - INTERVAL '8 weeks', now(), force => true);
--   CALL refresh_continuous_aggregate('prices_1mo', now() - INTERVAL '6 months', now(), force => true);
--   CALL refresh_continuous_aggregate('twap_1h', now() - INTERVAL '7 days', now(), force => true);
--   CALL refresh_continuous_aggregate('twap_1d', now() - INTERVAL '7 days', now(), force => true);
--   CALL refresh_continuous_aggregate('pools_per_source_1h', now() - INTERVAL '7 days', now(), force => true);
--
-- then walk older windows the same way, prices_1m first for each window.
-- Once pools_per_source_1h is whole, restore its real-time tail (0076):
--
--   ALTER MATERIALIZED VIEW pools_per_source_1h SET (timescaledb.materialized_only = false);
--
-- The NULL-start TWAP refresh stays forbidden (0156):
--
--   DO NOT RUN: CALL refresh_continuous_aggregate('twap_1h', NULL, now());

BEGIN;

-- Dependents first: twap_1h / twap_1d are hierarchical over prices_1m.
DROP MATERIALIZED VIEW IF EXISTS twap_1d;    -- migration-compat:ok recreated with 0166's columns; the released binary reads named columns only
DROP MATERIALIZED VIEW IF EXISTS twap_1h;    -- migration-compat:ok recreated with 0166's columns; the released binary reads named columns only
DROP MATERIALIZED VIEW IF EXISTS prices_1mo; -- migration-compat:ok recreated with 0147's columns plus two appended; the released binary reads named columns only
DROP MATERIALIZED VIEW IF EXISTS prices_1w;  -- migration-compat:ok recreated with 0147's columns plus two appended; the released binary reads named columns only
DROP MATERIALIZED VIEW IF EXISTS prices_1d;  -- migration-compat:ok recreated with 0147's columns plus two appended; the released binary reads named columns only
DROP MATERIALIZED VIEW IF EXISTS prices_4h;  -- migration-compat:ok recreated with 0147's columns plus two appended; the released binary reads named columns only
DROP MATERIALIZED VIEW IF EXISTS prices_1h;  -- migration-compat:ok recreated with 0147's columns plus two appended; the released binary reads named columns only
DROP MATERIALIZED VIEW IF EXISTS prices_15m; -- migration-compat:ok recreated with 0147's columns plus two appended; the released binary reads named columns only
DROP MATERIALIZED VIEW IF EXISTS prices_1m;  -- migration-compat:ok recreated with 0166's columns plus two appended; the released binary reads named columns only
DROP MATERIALIZED VIEW IF EXISTS pools_per_source_1h; -- migration-compat:ok recreated with 0036's columns; the released binary reads named columns only

CREATE MATERIALIZED VIEW prices_1m
WITH (timescaledb.continuous, timescaledb.materialized_only = true) AS
SELECT
    time_bucket('1 minute', ts)                                          AS bucket,
    base_asset,
    quote_asset,
    sum(quote_amount) FILTER (WHERE base_amount > 0 AND quote_amount > 0)
      / sum(base_amount) FILTER (WHERE base_amount > 0 AND quote_amount > 0) AS vwap,
    COALESCE(avg(quote_amount / base_amount)
               FILTER (WHERE usd_volume >= 0.01 AND base_amount > 0 AND quote_amount > 0),
             avg(quote_amount / base_amount)
               FILTER (WHERE base_amount > 0 AND quote_amount > 0))      AS twap,
    sum(base_amount)                                                     AS volume,
    sum(coalesce(usd_volume, 0))                                         AS volume_usd,
    count(*)                                                             AS trade_count,
    array_agg(DISTINCT source)                                           AS sources,
    COALESCE(first(quote_amount / base_amount,
                   lpad((extract(epoch FROM ts) * 1000000)::bigint::text, 19, '0')
                   || lpad(ledger::text, 10, '0') || tx_hash
                   || lpad(op_index::text, 10, '0') || source)
                   FILTER (WHERE usd_volume >= 0.01 AND base_amount > 0 AND quote_amount > 0),
             first(quote_amount / base_amount,
                   lpad((extract(epoch FROM ts) * 1000000)::bigint::text, 19, '0')
                   || lpad(ledger::text, 10, '0') || tx_hash
                   || lpad(op_index::text, 10, '0') || source)
                   FILTER (WHERE base_amount > 0 AND quote_amount > 0))  AS first_price,
    COALESCE(last(quote_amount / base_amount,
                  lpad((extract(epoch FROM ts) * 1000000)::bigint::text, 19, '0')
                  || lpad(ledger::text, 10, '0') || tx_hash
                  || lpad(op_index::text, 10, '0') || source)
                  FILTER (WHERE usd_volume >= 0.01 AND base_amount > 0 AND quote_amount > 0),
             last(quote_amount / base_amount,
                  lpad((extract(epoch FROM ts) * 1000000)::bigint::text, 19, '0')
                  || lpad(ledger::text, 10, '0') || tx_hash
                  || lpad(op_index::text, 10, '0') || source)
                  FILTER (WHERE base_amount > 0 AND quote_amount > 0))   AS last_price,
    COALESCE(max(quote_amount / base_amount)
               FILTER (WHERE usd_volume >= 0.01 AND base_amount > 0 AND quote_amount > 0),
             max(quote_amount / base_amount)
               FILTER (WHERE base_amount > 0 AND quote_amount > 0))      AS high_price,
    COALESCE(min(quote_amount / base_amount)
               FILTER (WHERE usd_volume >= 0.01 AND base_amount > 0 AND quote_amount > 0),
             min(quote_amount / base_amount)
               FILTER (WHERE base_amount > 0 AND quote_amount > 0))      AS low_price,
    count(*) FILTER (WHERE usd_volume >= 0.01
                       AND base_amount > 0 AND quote_amount > 0)         AS notional_trade_count,
    sum(quote_amount)                                                    AS volume_quote,
    sum(base_amount) FILTER (WHERE base_amount > 0 AND quote_amount > 0) AS volume_priced
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

-- 0156's retention policy went with the dropped view. Same horizon,
-- shipped disarmed, and the disarm is asserted, not assumed.
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

DO $do$
DECLARE
    -- 0115's notional floor, mirrored by usd_fx_resolver.go's bridgeLegMinUSDVolume.
    dust_filter text := 'FILTER (WHERE usd_volume >= 0.01 AND base_amount > 0 AND quote_amount > 0)';
    priceable   text := 'FILTER (WHERE base_amount > 0 AND quote_amount > 0)';

    -- 0147's total-order tie-break key.
    tie_key text := $k$(lpad((extract(epoch FROM ts) * 1000000)::bigint::text, 19, '0')
                        || lpad(ledger::text, 10, '0')
                        || tx_hash
                        || lpad(op_index::text, 10, '0')
                        || source)$k$;

    g           record;
    view_name   text;
BEGIN
    FOR g IN
        SELECT *
          FROM (VALUES
            -- grain, bucket expression,           start_offset, end_offset,   schedule
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
            WITH (timescaledb.continuous, timescaledb.materialized_only = true) AS
            SELECT
                %2$s                                                                 AS bucket,
                base_asset,
                quote_asset,
                sum(quote_amount) %5$s / sum(base_amount) %5$s                       AS vwap,
                avg( quote_amount / base_amount ) %5$s                               AS twap,
                sum(base_amount)                                                     AS volume,
                sum(coalesce(usd_volume, 0))                                         AS volume_usd,
                count(*)                                                             AS trade_count,
                array_agg(DISTINCT source)                                           AS sources,
                COALESCE(first(quote_amount / base_amount, %4$s) %3$s,
                         first(quote_amount / base_amount, %4$s) %5$s)               AS first_price,
                COALESCE(last (quote_amount / base_amount, %4$s) %3$s,
                         last (quote_amount / base_amount, %4$s) %5$s)               AS last_price,
                COALESCE(max  (quote_amount / base_amount)       %3$s,
                         max  (quote_amount / base_amount)       %5$s)               AS high_price,
                COALESCE(min  (quote_amount / base_amount)       %3$s,
                         min  (quote_amount / base_amount)       %5$s)               AS low_price,
                sum(quote_amount)                                                    AS volume_quote,
                sum(base_amount) %5$s                                                AS volume_priced
            FROM trades
            GROUP BY bucket, base_asset, quote_asset
            WITH NO DATA
        $f$, view_name, g.bucket_expr, dust_filter, tie_key, priceable);

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

CREATE MATERIALIZED VIEW twap_1h
WITH (timescaledb.continuous, timescaledb.materialized_only = true) AS
SELECT
    time_bucket('1 hour', bucket)   AS bucket,
    base_asset,
    quote_asset,
    COALESCE(avg(twap) FILTER (WHERE notional_trade_count > 0),
             avg(twap))                                          AS twap,
    sum(volume)                     AS volume,
    sum(volume_usd)                 AS volume_usd,
    sum(trade_count)                AS trade_count,
    COALESCE(NULLIF(count(*) FILTER (WHERE notional_trade_count > 0), 0),
             count(twap))                                        AS sample_count,
    count(*) FILTER (WHERE notional_trade_count > 0)             AS notional_sample_count
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
WITH (timescaledb.continuous, timescaledb.materialized_only = true) AS
SELECT
    time_bucket('1 day', bucket)    AS bucket,
    base_asset,
    quote_asset,
    COALESCE(avg(twap) FILTER (WHERE notional_trade_count > 0),
             avg(twap))                                          AS twap,
    sum(volume)                     AS volume,
    sum(volume_usd)                 AS volume_usd,
    sum(trade_count)                AS trade_count,
    COALESCE(NULLIF(count(*) FILTER (WHERE notional_trade_count > 0), 0),
             count(twap))                                        AS sample_count,
    count(*) FILTER (WHERE notional_trade_count > 0)             AS notional_sample_count
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

CREATE MATERIALIZED VIEW pools_per_source_1h
WITH (timescaledb.continuous, timescaledb.materialized_only = true) AS
SELECT
    time_bucket('1 hour', ts)                                   AS bucket,
    source,
    base_asset,
    quote_asset,
    sum(coalesce(usd_volume, 0)::numeric)                       AS sum_usd_priced,
    sum(base_amount)  FILTER (WHERE usd_volume IS NULL)         AS sum_base_unpriced,
    sum(quote_amount) FILTER (WHERE usd_volume IS NULL)         AS sum_quote_unpriced,
    count(*)                                                    AS trade_count,
    last(quote_amount / base_amount, ts)
      FILTER (WHERE base_amount > 0 AND quote_amount > 0)       AS bucket_last_price,
    last(ts, ts)                                                AS bucket_last_ts
FROM trades
GROUP BY bucket, source, base_asset, quote_asset
WITH NO DATA;

CREATE INDEX pools_per_source_1h_lookup_idx
    ON pools_per_source_1h (source, base_asset, quote_asset, bucket DESC);

SELECT add_continuous_aggregate_policy(
    'pools_per_source_1h',
    start_offset       => INTERVAL '7 days',
    end_offset         => INTERVAL '5 minutes',
    schedule_interval  => INTERVAL '5 minutes'
);

COMMIT;
