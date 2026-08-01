-- 0126 up — add `sample_count` (per-direction MINUTE COVERAGE) to the
-- twap_1h / twap_1d continuous aggregates (audit finding M-B; MNY-06
-- sibling on the TWAP surface).
--
-- ─── The defect ────────────────────────────────────────────────────────
-- SDEX stores the same market in BOTH orientations (the decoder files
-- each trade in the venue's observed base/quote ordering — XLM/USDC AND
-- USDC/XLM land as separate CAGG rows), so every serving read has to fold
-- the two directions into the orientation the caller asked for.
--
-- For TWAP that fold was TRADE-COUNT-weighted
-- (internal/storage/timescale/aggregates.go::TWAPPointsInRange, pre-fix:
-- `SUM((CASE … 1.0/twap …) * trade_count) / SUM(trade_count)`). That is
-- exactly the weighting migration 0081 exists to REJECT: twap_1h.twap =
-- avg(prices_1m.twap) is EQUAL PER ELAPSED MINUTE, deliberately NOT per
-- trade (a minute with 1000 dust prints must not outvote 999 quiet
-- minutes). Weighting the DIRECTION merge by trade count reintroduces that
-- bias across the two sides of a two-sided market: it is exact only in the
-- degenerate case where each direction's trade count is proportional to
-- its minute coverage, and wrong by an unbounded factor otherwise. Equal-
-- weighting the two directions is ALSO wrong — it regresses the healthy,
-- well-covered case — so the only correct merge weight is each direction's
-- minute COVERAGE: how many prices_1m minute-buckets it contributed.
--
-- That count is NOT recoverable from the stored twap/volume/volume_usd/
-- trade_count columns, so it has to be materialized. This migration adds
-- it as ONE new column, `sample_count`, to each TWAP CAGG.
--
-- ─── What sample_count is ──────────────────────────────────────────────
--   twap_1h.sample_count = count(*) over the ≤60 prices_1m minute-buckets
--                          in the hour (per base_asset/quote_asset)
--   twap_1d.sample_count = count(*) over the ≤1440 prices_1m minute-buckets
--                          in the day  (per base_asset/quote_asset)
-- i.e. the exact denominator of each view's own `avg(twap)`. Both views
-- read prices_1m DIRECTLY (0081 / 0115), so a plain `count(*)` in the same
-- GROUP BY is the per-direction minute-coverage count the serve-layer fold
-- (combineDirTWAP) weights by:
--
--   combined_twap = Σ(oriented_twap · sample_count) / Σ(sample_count)
--
-- ─── Why the whole view is recreated ───────────────────────────────────
-- A continuous aggregate's SELECT cannot be ALTERed; adding a column means
-- DROP + CREATE (the migration 0115 pattern). Unlike 0115 this touches
-- ONLY the two hierarchical TWAP views over prices_1m — the seven
-- prices_* views (and their ~1.1 TB) are untouched, so re-materialization
-- here is just the two small TWAP roll-ups.
--
-- Everything else about twap_1h / twap_1d is preserved VERBATIM from
-- 0081/0115: column list + order (sample_count is APPENDED after
-- trade_count, so every prior column keeps its position), the
-- `time_bucket(…, bucket)` grouping, the `_pair_bucket_idx` indexes, the
-- add_continuous_aggregate_policy offsets + schedule, WITH NO DATA, and NO
-- retention (0081/ADR-0034 — indefinite). `materialized_only` is captured
-- before the drop and re-applied after the create so this recreate cannot
-- silently flip real-time aggregation on a DB whose views predate a
-- TimescaleDB default change (0115 discipline).
--
-- OLD-BINARY-SAFE (rule 9): the recreated views are a column SUPERSET of
-- 0081/0115 — the previous released binary reads twap/volume/volume_usd/
-- trade_count by NAME and never SELECTs sample_count, so it keeps working
-- against the new schema until the new binary ships.
--
-- ─── ⚠ OPERATOR: this migration leaves twap_1h / twap_1d EMPTY ─────────
-- WITH NO DATA. Applying it DELETES the materialized TWAP history of the
-- two views, and the API serves no TWAP bars for a pair/grain until it is
-- re-materialized. Re-materialization is an OPERATOR step, run AFTER
-- prices_1m is whole (these are hierarchical CAGGs over prices_1m) and NOT
-- performed here:
--
--   CALL refresh_continuous_aggregate('twap_1h', NULL, now());
--   CALL refresh_continuous_aggregate('twap_1d', NULL, now());
--
-- Budget minutes, not hours — prices_1m is already materialized and these
-- roll-ups are small relative to the seven price views.

BEGIN;

-- Capture the current real-time-aggregation setting of the two views this
-- migration recreates, so the recreate cannot silently change it.
CREATE TEMP TABLE _m_b_prev_matonly ON COMMIT DROP AS
SELECT view_name, materialized_only
  FROM timescaledb_information.continuous_aggregates
 WHERE view_schema = 'public'
   AND view_name IN ('twap_1h', 'twap_1d');

-- Dropping a materialized view takes its policies and indexes with it.
-- Neither TWAP view depends on the other (both read prices_1m directly),
-- but the recreate below is a strict column-superset of 0081/0115, so the
-- previously-released binary's named-column read is unaffected.
DROP MATERIALIZED VIEW IF EXISTS twap_1d;  -- migration-compat:ok recreated as a column-superset (adds sample_count); no released binary reads it
DROP MATERIALIZED VIEW IF EXISTS twap_1h;  -- migration-compat:ok recreated as a column-superset (adds sample_count); no released binary reads it

-- 1-hour TWAP — hierarchical over prices_1m, RETAINED INDEFINITELY.
-- Identical to 0081/0115 plus the appended `sample_count`.
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

-- 1-day TWAP — hierarchical over prices_1m, RETAINED INDEFINITELY.
-- Identical to 0081/0115 plus the appended `sample_count`.
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

-- Restore each view's previous real-time-aggregation setting (no-op when
-- it already matches this TimescaleDB's default).
DO $do$
DECLARE
    v record;
BEGIN
    FOR v IN SELECT * FROM _m_b_prev_matonly LOOP
        EXECUTE format(
            'ALTER MATERIALIZED VIEW %I SET (timescaledb.materialized_only = %L)',
            v.view_name, v.materialized_only);
    END LOOP;
END
$do$;

COMMIT;
