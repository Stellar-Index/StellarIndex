-- 0147 up — deterministic open/close tie-break in the price CAGGs, and
-- the exact single-division VWAP (the 0115-invited free rider).
--
-- ─── The defect ────────────────────────────────────────────────────────
-- The served OHLC open/close are `first/last(quote_amount/base_amount, ts)`
-- (migrations 0002/0115). `ts` is the ledger close time — WHOLE SECONDS —
-- so every bucket in which two trades share a close (same ledger, or two
-- ledgers closing within the same second) has a TIE, and TimescaleDB's
-- first()/last() resolve ties by physical scan order: whichever heap/
-- chunk order the refresh happened to read. Two re-materializations of
-- the same bucket can serve DIFFERENT opens/closes; a replay or a chunk
-- recompression can silently flip a bar that already served. For a
-- showcase product whose bars must be reproducible bit-for-bit, "depends
-- on scan order" is a correctness bug even when both candidate prices
-- are individually real (determinism sweep 2026-08-21; the serve-layer
-- sibling fix — raw-trades ORDER BY tie-break — landed in v0.39.1).
--
-- ─── The fix ───────────────────────────────────────────────────────────
-- first()/last() accept ANY comparable type as their ordering argument,
-- so give them a TOTAL order: a fixed-width text key
--
--   lpad(epoch(ts),12) ‖ lpad(ledger,10) ‖ tx_hash ‖ lpad(op_index,10) ‖ source
--
-- computed inline in the CAGG SELECT — NO schema change to the (85 GB,
-- compressed) trades hypertable, no backfilled column. The key is total
-- because (ledger, tx_hash, op_index, source) uniquely identifies a
-- trade row (the trades PK components), and it EXACTLY mirrors the
-- serve layer's raw-trades ordering (TradesInRange:
-- `ORDER BY ts, ledger, tx_hash, op_index, source` — v0.39.1), so the
-- open/close of a bar always equals the first/last row a client would
-- page through for the same window. All five components are NOT NULL;
-- tx_hash is fixed-width char(64); lpad widths cover the full int4
-- range, so lexicographic text order == the intended numeric order.
--
-- ─── Free rider: exact VWAP (single division) ──────────────────────────
-- migrations/README.md rule 8 mandates `sum(quote_amount)/sum(base_amount)`
-- for new aggregates and records that the legacy per-row form in these
-- seven CAGGs diverges by ≤1.0e-16 relative — "NOT worth rematerializing
-- seven indefinite CAGGs" on its own, with 0115 explicitly inviting the
-- switch "as a free rider IF another full re-materialization is ever
-- scheduled". This migration IS that re-materialization, so the exact
-- form lands here. No served number changes beyond the 12-decimal wire
-- truncation (measured r1 2026-07-02, 40,565 bucket comparisons).
--
-- Everything else is preserved exactly from 0115/0126: column lists and
-- order, the dust-floor FILTER + COALESCE fallback on all four extremes
-- (B11-F1), policies, indexes, no retention (0031), the materialized_only
-- capture/restore, and the twap_1h/twap_1d dependents in their CURRENT
-- 0126 form (with sample_count).
--
-- ─── ⚠ OPERATOR: this migration leaves the CAGGs EMPTY ────────────────
-- WITH NO DATA — the 0115 pattern. All nine views are materialized_only,
-- so a pair/grain serves no bars until re-materialized. Recent-first:
--
--   CALL refresh_continuous_aggregate('prices_1m',  now() - INTERVAL '7 days', now());
--   CALL refresh_continuous_aggregate('prices_15m', now() - INTERVAL '30 days', now());
--   CALL refresh_continuous_aggregate('prices_1h',  NULL, now());
--   CALL refresh_continuous_aggregate('prices_4h',  NULL, now());
--   CALL refresh_continuous_aggregate('prices_1d',  NULL, now());
--   CALL refresh_continuous_aggregate('prices_1w',  NULL, now());
--   CALL refresh_continuous_aggregate('prices_1mo', NULL, now());
--   CALL refresh_continuous_aggregate('prices_1m',  NULL, now() - INTERVAL '7 days');
--   CALL refresh_continuous_aggregate('prices_15m', NULL, now() - INTERVAL '30 days');
--   -- twap_* are hierarchical: refresh AFTER prices_1m is whole.
--   CALL refresh_continuous_aggregate('twap_1h', NULL, now());
--   CALL refresh_continuous_aggregate('twap_1d', NULL, now());
--
-- Budget hours; run off-peak. Storage: the four extremes' partials now
-- carry the ~100-byte text key instead of an 8-byte timestamp — a modest
-- increase on prices_1m, negligible on the coarser grains.

BEGIN;

CREATE TEMP TABLE _0147_prev_matonly ON COMMIT DROP AS
SELECT view_name, materialized_only
  FROM timescaledb_information.continuous_aggregates
 WHERE view_schema = 'public'
   AND view_name IN ('prices_1m', 'prices_15m', 'prices_1h', 'prices_4h',
                     'prices_1d', 'prices_1w', 'prices_1mo',
                     'twap_1h', 'twap_1d');

-- Dependents first (hierarchical CAGGs over prices_1m), then the price
-- views. Every view is recreated below with an IDENTICAL column list, so
-- no released binary can be broken by the swap.
DROP MATERIALIZED VIEW IF EXISTS twap_1d;  -- migration-compat:ok recreated verbatim (0126 definition, identical columns)
DROP MATERIALIZED VIEW IF EXISTS twap_1h;  -- migration-compat:ok recreated verbatim (0126 definition, identical columns)
DROP MATERIALIZED VIEW IF EXISTS prices_1mo; -- migration-compat:ok recreated with identical columns (tie-break + exact VWAP are value-level changes)
DROP MATERIALIZED VIEW IF EXISTS prices_1w;  -- migration-compat:ok recreated with identical columns (tie-break + exact VWAP are value-level changes)
DROP MATERIALIZED VIEW IF EXISTS prices_1d;  -- migration-compat:ok recreated with identical columns (tie-break + exact VWAP are value-level changes)
DROP MATERIALIZED VIEW IF EXISTS prices_4h;  -- migration-compat:ok recreated with identical columns (tie-break + exact VWAP are value-level changes)
DROP MATERIALIZED VIEW IF EXISTS prices_1h;  -- migration-compat:ok recreated with identical columns (tie-break + exact VWAP are value-level changes)
DROP MATERIALIZED VIEW IF EXISTS prices_15m; -- migration-compat:ok recreated with identical columns (tie-break + exact VWAP are value-level changes)
DROP MATERIALIZED VIEW IF EXISTS prices_1m;  -- migration-compat:ok recreated with identical columns (tie-break + exact VWAP are value-level changes)

-- The seven price CAGGs, table-driven (0115 pattern) so the definitions
-- cannot drift and the dust floor + tie-break key each appear ONCE.
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
    tie_key text := $k$(lpad(extract(epoch FROM ts)::bigint::text, 12, '0')
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

-- twap_1h / twap_1d — recreated verbatim from migration 0126 (collateral
-- of the prices_1m drop, not a change; sample_count preserved).
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

-- Restore each view's previous real-time-aggregation setting.
DO $do$
DECLARE
    v record;
BEGIN
    FOR v IN SELECT * FROM _0147_prev_matonly LOOP
        EXECUTE format(
            'ALTER MATERIALIZED VIEW %I SET (timescaledb.materialized_only = %L)',
            v.view_name, v.materialized_only);
    END LOOP;
END
$do$;

COMMIT;
