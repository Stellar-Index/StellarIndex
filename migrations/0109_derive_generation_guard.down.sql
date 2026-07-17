-- 0109 down — drop `derive_generation` from the three core tables,
-- reverting the schema half of the INV-3 generation-guarded upsert.
--
-- The post-0109 binary references derive_generation in its writers, so a
-- rollback of this migration must be paired with a rollback of the binary
-- to the pre-0109 (DO NOTHING) writers. DROP COLUMN on a compressed
-- hypertable is supported directly on TimescaleDB 2.11+ (r1 runs 2.26).

BEGIN;

ALTER TABLE asset_supply_history
    DROP COLUMN IF EXISTS derive_generation;

ALTER TABLE oracle_updates
    DROP COLUMN IF EXISTS derive_generation;

ALTER TABLE trades
    DROP COLUMN IF EXISTS derive_generation;

COMMIT;
