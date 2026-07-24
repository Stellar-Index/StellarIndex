-- 0115 up — notional floor on the OHLC extremes in the price CAGGs
-- (audit finding B11-F1; docs/operations/finding-dust-trades-set-chart-extremes.md).
--
-- ─── The defect ────────────────────────────────────────────────────────
-- Migration 0002 computed open/high/low/close over EVERY trade in the
-- bucket regardless of notional. A quarter of all Stellar trades are
-- sub-cent dust — mostly path-payment remainders, the few-stroop crumb
-- left after the earlier hops consumed the book — and at that size the
-- "price" is an artifact of dividing two tiny integers, not an
-- observation of the market.
--
-- Measured on production (2026-07-17 06:00 UTC, XLM/USD 1h bar): the
-- served low was 0.1333333333. That is 1 / 7.5, and the 7.5 came from a
-- SINGLE `USDC-GA5Z…/native` fill of 2 stroops for 15 stroops —
-- usd_volume $0.00000027. The real market low that hour was 0.1822
-- (CEX range 0.1822–0.1836). Same class of print put a 0.2000 "high" on
-- the 07-15 and 07-16 daily bars against a real max of ~0.1848.
--
-- ─── The fix ───────────────────────────────────────────────────────────
-- Every extreme is computed over economically meaningful trades only:
--
--   COALESCE(max(price) FILTER (WHERE usd_volume >= 0.01), max(price))
--
-- This MUST live in the continuous aggregate. The CAGG stores only the
-- already-collapsed extremes, so no serve-layer filter can recover a
-- price the aggregate has already thrown away. (Verified on TimescaleDB
-- 2.26.4 — the deployed version — that FILTER + COALESCE are accepted
-- inside a continuous aggregate for first/last/max/min.)
--
-- The COALESCE fallback is load-bearing: a bucket containing ONLY dust
-- must still report an extreme rather than NULL, which would blank the
-- bar instead of showing the (admittedly poor) price we actually saw.
--
-- Applied to all four extremes, not just high/low: `first_price` /
-- `last_price` are the served OHLC open/close and a dust print that
-- happens to be first or last in the bucket pins them exactly the same
-- way. VWAP, TWAP, volume, volume_usd, trade_count and sources are
-- DELIBERATELY unfiltered — those are census/weighted quantities where
-- a crumb contributes ~0 by construction, and filtering them would
-- change volume semantics (a $0.00000027 fill is still a trade that
-- happened).
--
-- ─── Why $0.01, and why a size floor rather than a price band ──────────
-- Operator DECISION, 2026-07-22 (see the finding, "DECISION"): filter on
-- trade SIZE, never on price divergence. A genuine fat-finger — a
-- $100,000 sale at a terrible price — is a real market event and MUST
-- still show; suppressing it because it sits far from VWAP is editing
-- reality. What must go is dust: fills whose total value is below the
-- point where price carries information.
--
-- $0.01 on the 2026-07-17 distribution (6,018,245 trades):
--
--   usd_volume < $0.01    1,448,695 trades   24.1% by COUNT
--                                            ~0%   by VALUE
--
-- Every observed wick sits orders of magnitude below the floor (the
-- 2↔15-stroop crumb by ~37,000×; the S-012 1↔1-stroop print by ~100,000×),
-- while both legitimate hops of the same path payment ($0.09 and $19.99)
-- are kept, so the genuine execution stays fully represented. A $100k
-- fat-finger clears the floor by 7 orders of magnitude and is SHOWN.
--
-- Consequence of that decision, applied in the same change: the 2×-VWAP
-- band in the serve layer (`combinedOutlierBandRatio`,
-- internal/api/v1/ohlc_fiat_combine.go) is REMOVED. It was treating the
-- symptom (distance from VWAP) when the cause was always size — it
-- missed this very wick (0.1333 is 0.73× VWAP, comfortably inside a 2×
-- band) and could clip a genuine large move. The notional floor subsumes
-- it.
--
-- usd_volume IS NULL never satisfies `>= 0.01`, so an entirely unpriced
-- pair falls through the COALESCE to the unfiltered extreme — today's
-- behaviour, deliberately unchanged (the finding's open question 2: a
-- stroop-based floor for unpriced pairs is a separate decision, and
-- keeping current behaviour is the option that cannot suppress a real
-- extreme). A bucket that MIXES priced and unpriced trades takes its
-- extremes from the priced ones only.
--
-- ─── Why the whole view is recreated ───────────────────────────────────
-- A continuous aggregate's SELECT cannot be ALTERed; changing it means
-- DROP + CREATE. `twap_1h` / `twap_1d` (migration 0081) are hierarchical
-- CAGGs reading prices_1m, so they are dependents and must be dropped
-- and recreated too — byte-identical to 0081, no semantic change; they
-- read twap/volume/volume_usd/trade_count, none of which this migration
-- touches.
--
-- The VWAP expression is preserved VERBATIM in its legacy per-row form
-- (`sum((quote/base)*base)/sum(base)`), deliberately and not by
-- oversight: migrations/README.md rule 8 records that the exact
-- single-division form differs from it by ≤1.0e-16 relative — below the
-- 12-decimal wire truncation, so switching it here would change no
-- served number while adding an unrelated money-formula change to a
-- dust-fix migration. It is worth revisiting as a free rider IF another
-- full re-materialization is ever scheduled.
--
-- Everything else about the seven price CAGGs is preserved exactly:
-- column list, order and semantics; the per-grain
-- add_continuous_aggregate_policy offsets and schedule; the
-- <view>_pair_bucket_idx indexes; WITH NO DATA. RETENTION IS
-- DELIBERATELY NOT RE-ADDED — migration 0031 removed the 30-day
-- retention on prices_1m/prices_15m and its `down` is an intentional
-- no-op, because ADR-0034 keeps the served history forever; re-arming
-- add_retention_policy here would resurrect the exact data-loss drift
-- 0031 exists to prevent. The pre-existing `materialized_only` setting
-- of each view is captured before the drop and re-applied after the
-- create, so this recreate cannot silently flip real-time aggregation on
-- a DB whose views were created under an older TimescaleDB default.
--
-- ─── ⚠ OPERATOR: this migration leaves the CAGGs EMPTY ────────────────
-- WITH NO DATA. Applying it DELETES the materialized OHLC history
-- (~1.1 TB across the seven price views) and the API serves no bars for
-- a pair/grain until it is re-materialized. Re-materialization is an
-- OPERATOR step and is NOT performed here:
--
--   -- recent first, so charts come back quickly:
--   CALL refresh_continuous_aggregate('prices_1m',  now() - INTERVAL '7 days', now());
--   CALL refresh_continuous_aggregate('prices_15m', now() - INTERVAL '30 days', now());
--   …then walk backwards in windows, coarse grains last:
--   CALL refresh_continuous_aggregate('prices_1h',  NULL, now());
--   CALL refresh_continuous_aggregate('prices_4h',  NULL, now());
--   CALL refresh_continuous_aggregate('prices_1d',  NULL, now());
--   CALL refresh_continuous_aggregate('prices_1w',  NULL, now());
--   CALL refresh_continuous_aggregate('prices_1mo', NULL, now());
--   -- twap_* are hierarchical: refresh them AFTER prices_1m is whole.
--   CALL refresh_continuous_aggregate('twap_1h', NULL, now());
--   CALL refresh_continuous_aggregate('twap_1d', NULL, now());
--
-- Budget hours, run it off the D2 window, and expect a modest storage
-- increase: each extreme now materializes two partials (filtered +
-- unfiltered fallback) instead of one.

BEGIN;

-- Capture the current real-time-aggregation setting of every view this
-- migration recreates, so the recreate cannot silently change it.
CREATE TEMP TABLE _b11f1_prev_matonly ON COMMIT DROP AS
SELECT view_name, materialized_only
  FROM timescaledb_information.continuous_aggregates
 WHERE view_schema = 'public'
   AND view_name IN ('prices_1m', 'prices_15m', 'prices_1h', 'prices_4h',
                     'prices_1d', 'prices_1w', 'prices_1mo',
                     'twap_1h', 'twap_1d');

-- Dependents first (hierarchical CAGGs over prices_1m), then the price
-- views themselves. Dropping a materialized view takes its policies and
-- indexes with it.
DROP MATERIALIZED VIEW IF EXISTS twap_1d;
DROP MATERIALIZED VIEW IF EXISTS twap_1h;
DROP MATERIALIZED VIEW IF EXISTS prices_1mo;
DROP MATERIALIZED VIEW IF EXISTS prices_1w;
DROP MATERIALIZED VIEW IF EXISTS prices_1d;
DROP MATERIALIZED VIEW IF EXISTS prices_4h;
DROP MATERIALIZED VIEW IF EXISTS prices_1h;
DROP MATERIALIZED VIEW IF EXISTS prices_15m;
DROP MATERIALIZED VIEW IF EXISTS prices_1m;

-- The seven price CAGGs, table-driven so the seven definitions cannot
-- drift from one another and the threshold appears EXACTLY ONCE.
DO $do$
DECLARE
    -- ── THE NOTIONAL FLOOR ── the single source of truth for the OHLC
    -- dust threshold. Change it here and nowhere else (then
    -- re-materialize). Mirrored, deliberately, by
    -- internal/storage/timescale/usd_fx_resolver.go's
    -- bridgeLegMinUSDVolume so the two dust defences agree.
    ohlc_extreme_min_usd_volume CONSTANT numeric := 0.01;

    dust_filter text := format('FILTER (WHERE usd_volume >= %L)',
                               ohlc_extreme_min_usd_volume);
    g           record;
    view_name   text;
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
            -- Timescale uses calendar-month bucketing; requires a timezone arg.
            ('1mo', $b$time_bucket('1 month', ts, 'UTC')$b$, '3 months', '1 day',     '1 day')
          ) AS t(grain, bucket_expr, start_offset, end_offset, schedule)
    LOOP
        view_name := 'prices_' || g.grain;

        -- Column list, order and semantics are identical to migration
        -- 0002 except the four extremes, which now prefer the
        -- above-floor trades and fall back to the unfiltered aggregate
        -- for an all-dust bucket.
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

-- twap_1h / twap_1d — recreated verbatim from migration 0081 (they were
-- collateral of the prices_1m drop, not a change).
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

-- Restore each view's previous real-time-aggregation setting (no-op when
-- it already matches this TimescaleDB's default).
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
