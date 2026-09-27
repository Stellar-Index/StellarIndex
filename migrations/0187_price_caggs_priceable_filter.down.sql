-- 0187 down — restore the pre-0187 definitions: prices_1m and
-- twap_1h/1d as 0166 left them, prices_15m..prices_1mo as 0147 did,
-- pools_per_source_1h as 0036 did and real-time again (0076). Drops the
-- volume_quote / volume_priced columns and the priceable filter, so it
-- is only safe while `trades` holds no zero-leg row: with one present,
-- the restored unfiltered division fails every refresh. Re-attaches
-- 0156's retention on prices_1m disarmed, as the up does. Leaves all
-- ten views EMPTY, like the up. Re-materialise them the same way:
-- recent first, prices_1m before the TWAP views, windowed and forced,
-- then walk older windows:
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
-- The NULL-start TWAP refresh stays forbidden (0156):
--
--   DO NOT RUN: CALL refresh_continuous_aggregate('twap_1h', NULL, now());

BEGIN;

DROP MATERIALIZED VIEW IF EXISTS twap_1d;    -- migration-compat:ok down-migration restore (0166 definition)
DROP MATERIALIZED VIEW IF EXISTS twap_1h;    -- migration-compat:ok down-migration restore (0166 definition)
DROP MATERIALIZED VIEW IF EXISTS prices_1mo; -- migration-compat:ok down-migration restore (0147 definition)
DROP MATERIALIZED VIEW IF EXISTS prices_1w;  -- migration-compat:ok down-migration restore (0147 definition)
DROP MATERIALIZED VIEW IF EXISTS prices_1d;  -- migration-compat:ok down-migration restore (0147 definition)
DROP MATERIALIZED VIEW IF EXISTS prices_4h;  -- migration-compat:ok down-migration restore (0147 definition)
DROP MATERIALIZED VIEW IF EXISTS prices_1h;  -- migration-compat:ok down-migration restore (0147 definition)
DROP MATERIALIZED VIEW IF EXISTS prices_15m; -- migration-compat:ok down-migration restore (0147 definition)
DROP MATERIALIZED VIEW IF EXISTS prices_1m;  -- migration-compat:ok down-migration restore (0166 definition)
DROP MATERIALIZED VIEW IF EXISTS pools_per_source_1h; -- migration-compat:ok down-migration restore (0036 definition)

CREATE MATERIALIZED VIEW prices_1m
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 minute', ts)                                          AS bucket,
    base_asset,
    quote_asset,
    sum(quote_amount) / sum(base_amount)                                 AS vwap,
    COALESCE(avg(quote_amount / base_amount) FILTER (WHERE usd_volume >= 0.01),
             avg(quote_amount / base_amount))                            AS twap,
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
             min(quote_amount / base_amount))                            AS low_price,
    count(*) FILTER (WHERE usd_volume >= 0.01)                           AS notional_trade_count
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

CREATE MATERIALIZED VIEW twap_1h
WITH (timescaledb.continuous) AS
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
             count(*))                                           AS sample_count,
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
WITH (timescaledb.continuous) AS
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
             count(*))                                           AS sample_count,
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

DO $do$
DECLARE
    -- ── THE NOTIONAL FLOOR ── unchanged from 0115 (B11-F1). Mirrored by
    -- internal/storage/timescale/usd_fx_resolver.go's bridgeLegMinUSDVolume.
    ohlc_extreme_min_usd_volume CONSTANT numeric := 0.01;

    dust_filter text := format('FILTER (WHERE usd_volume >= %L)',
                               ohlc_extreme_min_usd_volume);

    -- ── THE TIE-BREAK KEY ── total order over trade rows; mirrors the
    -- serve layer's TradesInRange ORDER BY (ts, ledger, tx_hash,
    -- op_index, source). Fixed-width so text order == numeric order.
    -- Epoch in MICROSECONDS (timestamptz's exact internal precision;
    -- numeric epoch * 1e6 is exact, ::bigint is then a no-op round) —
    -- seconds granularity would round sub-second CEX timestamps
    -- together and invert their true order (see header).
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
            -- Timescale uses calendar-month bucketing; requires a timezone arg.
            ('1mo', $b$time_bucket('1 month', ts, 'UTC')$b$, '3 months', '1 day',     '1 day')
          ) AS t(grain, bucket_expr, start_offset, end_offset, schedule)
    LOOP
        view_name := 'prices_' || g.grain;

        -- Column list, order and semantics identical to 0115 except:
        -- vwap is the exact single-division form (rule 8), and
        -- first/last order by the total tie-break key instead of ts.
        EXECUTE format($f$
            CREATE MATERIALIZED VIEW %1$I
            WITH (timescaledb.continuous) AS
            SELECT
                %2$s                                                                 AS bucket,
                base_asset,
                quote_asset,
                sum(quote_amount) / sum(base_amount)                                 AS vwap,
                avg( quote_amount / base_amount )                                    AS twap,
                sum(base_amount)                                                     AS volume,
                sum(coalesce(usd_volume, 0))                                         AS volume_usd,
                count(*)                                                             AS trade_count,
                array_agg(DISTINCT source)                                           AS sources,
                COALESCE(first(quote_amount / base_amount, %4$s) %3$s,
                         first(quote_amount / base_amount, %4$s))                    AS first_price,
                COALESCE(last (quote_amount / base_amount, %4$s) %3$s,
                         last (quote_amount / base_amount, %4$s))                    AS last_price,
                COALESCE(max  (quote_amount / base_amount)       %3$s,
                         max  (quote_amount / base_amount))                          AS high_price,
                COALESCE(min  (quote_amount / base_amount)       %3$s,
                         min  (quote_amount / base_amount))                          AS low_price
            FROM trades
            GROUP BY bucket, base_asset, quote_asset
            WITH NO DATA
        $f$, view_name, g.bucket_expr, dust_filter, tie_key);

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


ALTER MATERIALIZED VIEW prices_1m  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW prices_15m SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW prices_1h  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW prices_4h  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW prices_1d  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW prices_1w  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW prices_1mo SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW twap_1h    SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW twap_1d    SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW pools_per_source_1h SET (timescaledb.materialized_only = false);

COMMIT;
