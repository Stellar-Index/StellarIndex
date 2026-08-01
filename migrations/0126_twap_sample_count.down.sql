-- 0126 down — drop `sample_count`, restoring the twap_1h / twap_1d
-- continuous aggregates to their 0081/0115 column set.
--
-- Reverses 0126 up by recreating the two hierarchical TWAP views WITHOUT
-- the `sample_count` column — byte-identical to migration 0081 (and its
-- verbatim recreate in 0115). A CAGG SELECT cannot be ALTERed, so removing
-- a column is DROP + CREATE, exactly like the up.
--
-- ⚠ This re-opens the M-B defect: with no per-direction minute-coverage
-- column, TWAPPointsInRange can only fall back to trade-count (or equal)
-- weighting of the two stored market directions, which migration 0081
-- exists to reject. Roll back only to unblock an incident, and prefer a
-- NEW forward migration to any lasting change. NOTE: the post-0126 binary
-- reads `sample_count`; run this `down` only alongside rolling the binary
-- back to a pre-0126 release (the rule-9 two-release contract in reverse).
--
-- ⚠ Like the up, this leaves twap_1h / twap_1d EMPTY (WITH NO DATA) — an
-- operator re-materialization is required afterwards:
--   CALL refresh_continuous_aggregate('twap_1h', NULL, now());
--   CALL refresh_continuous_aggregate('twap_1d', NULL, now());
-- (run AFTER prices_1m is whole; these are hierarchical CAGGs over it).
--
-- RETENTION IS DELIBERATELY NOT RE-ADDED: 0081 carries none (indefinite by
-- design, ADR-0034); this restores the 0081 definitions, nothing more.

BEGIN;

CREATE TEMP TABLE _m_b_prev_matonly ON COMMIT DROP AS
SELECT view_name, materialized_only
  FROM timescaledb_information.continuous_aggregates
 WHERE view_schema = 'public'
   AND view_name IN ('twap_1h', 'twap_1d');

DROP MATERIALIZED VIEW IF EXISTS twap_1d;
DROP MATERIALIZED VIEW IF EXISTS twap_1h;

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
    FOR v IN SELECT * FROM _m_b_prev_matonly LOOP
        EXECUTE format(
            'ALTER MATERIALIZED VIEW %I SET (timescaledb.materialized_only = %L)',
            v.view_name, v.materialized_only);
    END LOOP;
END
$do$;

COMMIT;
