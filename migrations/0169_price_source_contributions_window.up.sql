-- 0169 up — give `price_source_contributions` the aggregation window it
-- was missing, and a declared (disabled) retention horizon.
--
-- The orchestrator records one contribution breakdown per (pair,
-- window) — 5m, 1h and 24h by default — but the table (0026) had no
-- window column and the aggregator's sink dropped the field, so the
-- three breakdowns landed as indistinguishable rows whose `bucket`
-- (the write's wall-clock time) differed only by the loop's
-- microseconds. "Latest row per (asset, quote, source)" returned
-- whichever window ran last, and nothing on the row said which.
--
-- window_seconds is the window in whole seconds (300 / 3600 / 86400),
-- the same key freeze_events.window_ladders uses (0163).
--
-- Additive and old-binary-safe (README rule 9):
--   - the column is NULLABLE. The previous binary INSERTs without it and
--     keeps working; rows written before this migration stay NULL,
--     which is the truth — their window is unrecoverable. The new
--     binary refuses to write a row without a window
--     (InsertPriceSourceContributions), so NULL means "pre-0169" only.
--   - the 0026 primary key (asset_id, quote_id, bucket, source) stays,
--     because the previous binary's ON CONFLICT names exactly it. The
--     window-aware key is added ALONGSIDE it as a unique index, which
--     is what the new binary's ON CONFLICT names. Dropping the old key,
--     SET NOT NULL and clearing the NULL rows is release N+2's
--     migration, once no deployed binary writes the old shape.
--
-- Retention: 90 days, matching prices_1m (0156) — the minute price this
-- breakdown sits beside. Shipped DISABLED, as 0156 is: arming a
-- scheduled delete is an operator act, not a deploy side effect. To arm:
--
--   SELECT alter_job(job_id, scheduled => true)
--     FROM timescaledb_information.jobs
--    WHERE proc_name = 'policy_retention'
--      AND hypertable_name = 'price_source_contributions';
--
-- Compression is NOT added here: 0026 made the table eligible and
-- scripts/ops/add-missing-compression-policies.sql attaches its policy
-- after the Phase D backfill, by design (that script's header).
--
-- Index build (README rule 10; 0037 is the recipe): the in-transaction
-- build blocks writes to the hypertable for its duration. On a populated
-- node pre-build it by hand first — hypertables refuse CONCURRENTLY, so
-- build per chunk:
--
--   CREATE UNIQUE INDEX price_source_contributions_window_key
--       ON price_source_contributions (asset_id, quote_id, window_seconds, source, bucket)
--       WITH (timescaledb.transaction_per_chunk);
--
-- then run `stellarindex-migrate up`: IF NOT EXISTS below is a no-op, and
-- lock_timeout fails the migration instead of queueing every writer.

BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE price_source_contributions
    ADD COLUMN IF NOT EXISTS window_seconds integer
        CONSTRAINT price_source_contributions_window_seconds_check
        CHECK (window_seconds IS NULL OR window_seconds > 0);

CREATE UNIQUE INDEX IF NOT EXISTS price_source_contributions_window_key
    ON price_source_contributions (asset_id, quote_id, window_seconds, source, bucket);

COMMENT ON COLUMN price_source_contributions.window_seconds IS
    'Aggregation window this breakdown was computed over, in whole seconds '
    '(300, 3600, 86400). NULL only on rows written before migration 0169, '
    'whose window was not recorded and cannot be recovered.';

COMMENT ON COLUMN price_source_contributions.bucket IS
    'Wall-clock time the aggregator computed this breakdown (not a closed '
    'bucket boundary). One row per source per (pair, window) per aggregator tick.';

COMMENT ON TABLE price_source_contributions IS
    'Per-source breakdown of each windowed VWAP, appended once per aggregator '
    'tick per (pair, window). Read the latest bucket per (asset_id, quote_id, '
    'window_seconds). Powers the source-contribution donut.';

SELECT add_retention_policy(
         'price_source_contributions',
         drop_after    => INTERVAL '90 days',
         if_not_exists => true);

SELECT alter_job(job_id, scheduled => false)
  FROM timescaledb_information.jobs
 WHERE proc_name = 'policy_retention'
   AND hypertable_name = 'price_source_contributions';

COMMIT;
