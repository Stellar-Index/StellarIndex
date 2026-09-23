-- 0165 up — put the $0.01 notional floor on the TWAP chain.
--
-- ─── The defect ────────────────────────────────────────────────────────
-- 0115 floored the OHLC extremes at usd_volume >= $0.01 and left TWAP
-- unfiltered on the claim that a crumb "contributes ~0 by construction".
-- That holds for VWAP (volume-weighted) and is false for TWAP, which is
-- EQUAL-weight at both levels of its chain:
--
--   prices_1m.twap   = avg(quote/base)  — one vote per TRADE, so a
--                      2-stroop path-payment remainder at price 7.5
--                      counts as much as a $1,000 fill at 5.49;
--   twap_1h / twap_1d = avg(prices_1m.twap) — one vote per MINUTE, so
--                      a minute holding nothing but that crumb counts
--                      as much as a minute of real trading. On a thin
--                      pair (most minutes empty) one dust print moves
--                      the hour's TWAP by an unbounded factor.
--
-- ─── The fix — the 0115 guarded-floor shape, at every level ────────────
--   prices_1m.twap        COALESCE(avg(price) FILTER (WHERE usd_volume >= 0.01), avg(price))
--   prices_1m             + notional_trade_count: trades clearing the floor
--   twap_1h/1d.twap       COALESCE(avg(twap) FILTER (WHERE notional_trade_count > 0), avg(twap))
--   twap_1h/1d            sample_count counts the minutes the twap averaged;
--                         + notional_sample_count: minutes that cleared the floor
--
-- The COALESCE fallback keeps an all-dust or unpriced (usd_volume NULL)
-- window reporting its unfiltered value rather than NULL — exactly
-- 0115's rule for the extremes. notional_sample_count lets the serve
-- layer (aggregates.go::combineDirTWAP) apply the same rule ACROSS the
-- two stored market directions: a direction that fell back is dropped
-- from the merge when the other one cleared the floor.
--
-- ─── Scope: prices_1m and the two TWAP views only ──────────────────────
-- TWAP is served only through twap_1h / twap_1d, which are built on
-- prices_1m (migrations/README.md rule 8: "never prices_1m.twap
-- directly"). The six coarser price CAGGs keep their unserved legacy
-- twap column; re-materialising them from `trades` for a column nobody
-- reads is the trade 0115 declined for the exact VWAP. Switch them as a
-- free rider at the next full price-CAGG rebuild.
--
-- Everything else in prices_1m is 0147's definition verbatim (exact
-- VWAP, tie-break key, floored extremes, index, refresh policy);
-- twap_1h/1d keep 0126's policies and index. Dropping prices_1m drops
-- 0156's retention policy with it, so it is re-attached here with the
-- same 90-day horizon and, like 0156, DISARMED and asserted so.
--
-- ─── ⚠ OPERATOR: this migration leaves all three views EMPTY ──────────
-- WITH NO DATA, the 0115/0147 pattern. Follow 0156's Recovery section:
-- the retention policy is already disarmed by this migration. Recent
-- first, prices_1m BEFORE the TWAP views, every refresh WINDOWED:
--
--   CALL refresh_continuous_aggregate('prices_1m', now() - INTERVAL '7 days', now(), force => true);
--   CALL refresh_continuous_aggregate('twap_1h', now() - INTERVAL '7 days', now(), force => true);
--   CALL refresh_continuous_aggregate('twap_1d', now() - INTERVAL '7 days', now(), force => true);
--
-- then walk older windows the same way, prices_1m first for each window,
-- back to the start of the TWAP history you want to keep. Re-arm the
-- retention policy (0156 header) only after the TWAP views are whole.
-- The NULL-start TWAP refresh stays forbidden (0156):
--
--   DO NOT RUN: CALL refresh_continuous_aggregate('twap_1h', NULL, now());

BEGIN;

CREATE TEMP TABLE _0165_prev_matonly ON COMMIT DROP AS
SELECT view_name, materialized_only
  FROM timescaledb_information.continuous_aggregates
 WHERE view_schema = 'public'
   AND view_name IN ('prices_1m', 'twap_1h', 'twap_1d');

-- Dependents first: twap_1h / twap_1d are hierarchical over prices_1m.
DROP MATERIALIZED VIEW IF EXISTS twap_1d;   -- migration-compat:ok recreated with 0126's columns plus one appended; the released binary reads named columns only
DROP MATERIALIZED VIEW IF EXISTS twap_1h;   -- migration-compat:ok recreated with 0126's columns plus one appended; the released binary reads named columns only
DROP MATERIALIZED VIEW IF EXISTS prices_1m; -- migration-compat:ok recreated with 0147's columns plus one appended; the released binary reads named columns only

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
    start_offset      => INTERVAL '5 minutes',
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
    v record;
BEGIN
    FOR v IN SELECT * FROM _0165_prev_matonly LOOP
        EXECUTE format(
            'ALTER MATERIALIZED VIEW %I SET (timescaledb.materialized_only = %L)',
            v.view_name, v.materialized_only);
    END LOOP;
END
$do$;

COMMIT;
