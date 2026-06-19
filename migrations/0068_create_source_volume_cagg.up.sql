-- 0068 up — `source_volume_1h` per-source hourly volume continuous aggregate.
--
-- Backs the source page's activity chart (24h + 7d windows of per-hour
-- trade count + USD volume). The live derivation scanned the raw trades
-- hypertable per request — ~18s for the heaviest source (SDEX) over 7d,
-- past the API's 8s ceiling; the all-source 7d variant timed out
-- outright. This CAGG rolls trades up to one row per (source, hour) so
-- the read is a few hundred rows instead of ~500k.
--
-- The XLM/USD multiply CANNOT live in the CAGG (it cross-references
-- prices_1m, which continuous aggregates can't join). So we materialize
-- the raw INPUTS the per-row CASE in GetSourceStats uses, and apply the
-- multiply at read time:
--   sum_usd_priced = SUM(usd_volume)                  -- trades already USD-valued
--   sum_xlm_base   = SUM(base_amount)  FILTER usd_volume NULL AND base XLM
--   sum_xlm_quote  = SUM(quote_amount) FILTER usd_volume NULL AND base NOT XLM AND quote XLM
--   trade_count    = COUNT(*)
-- read query: sum_usd_priced + (sum_xlm_base + sum_xlm_quote)/1e7 * <current XLM/USD vwap>.
-- 'CAS3J7…' is the native-XLM SAC (same constant as GetSourceStats); the
-- base-XLM branch takes precedence over quote-XLM, matching the CASE.
--
-- WITH NO DATA: the create does not backfill. After applying, materialize
-- the window once (operator step — see release notes):
--   CALL refresh_continuous_aggregate('source_volume_1h', now() - interval '14 days', now());
-- The 5-minute policy below keeps the trailing 7d current thereafter
-- (older buckets stay materialized once the initial refresh writes them).

BEGIN;

CREATE MATERIALIZED VIEW source_volume_1h
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 hour', ts)  AS bucket,
    source,
    count(*)                   AS trade_count,
    sum(usd_volume)            AS sum_usd_priced,
    sum(base_amount) FILTER (
        WHERE usd_volume IS NULL
          AND base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
    )                          AS sum_xlm_base,
    sum(quote_amount) FILTER (
        WHERE usd_volume IS NULL
          AND base_asset NOT IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
          AND quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
    )                          AS sum_xlm_quote
FROM trades
GROUP BY bucket, source
WITH NO DATA;

CREATE INDEX source_volume_1h_lookup_idx
    ON source_volume_1h (source, bucket DESC);

-- Refresh the trailing 7d every 5 minutes (matches prices_1m / the
-- pools CAGG cadence; tolerates late-arriving backfilled trades).
SELECT add_continuous_aggregate_policy(
    'source_volume_1h',
    start_offset       => INTERVAL '7 days',
    end_offset         => INTERVAL '5 minutes',
    schedule_interval  => INTERVAL '5 minutes'
);

COMMIT;
