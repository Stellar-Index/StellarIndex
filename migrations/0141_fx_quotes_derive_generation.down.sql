-- 0141 down — drop `derive_generation` from fx_quotes, reverting the
-- schema half of the MR-1 generation-guarded upsert.
--
-- The post-0141 binary references derive_generation in InsertFXQuoteBatch,
-- so a rollback of this migration must be paired with a rollback of the
-- binary to the pre-0141 (unguarded DO UPDATE) writer. DROP COLUMN is
-- metadata-only on modern PostgreSQL and supported directly on
-- TimescaleDB 2.11+ (r1 runs 2.26) for a compressed hypertable.

BEGIN;

ALTER TABLE fx_quotes DROP COLUMN IF EXISTS derive_generation;

COMMIT;
