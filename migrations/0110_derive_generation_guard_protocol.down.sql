-- 0110 down — drop `derive_generation` from the protocol projector tables,
-- reverting the schema half of the INV-3 wave-2 generation-guarded upsert.
--
-- The post-0110 binary references derive_generation in these writers, so a
-- rollback of this migration must be paired with a rollback of the binary to
-- the pre-0110 (DO NOTHING) writers. DROP COLUMN is metadata-only on modern
-- PostgreSQL and supported directly on TimescaleDB 2.11+ (r1 runs 2.26) for
-- any of these that is a hypertable.

BEGIN;

ALTER TABLE sep41_supply_events DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE sep41_transfers     DROP COLUMN IF EXISTS derive_generation;

ALTER TABLE comet_liquidity DROP COLUMN IF EXISTS derive_generation;

ALTER TABLE soroswap_router_swaps DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE soroswap_skim_events  DROP COLUMN IF EXISTS derive_generation;

ALTER TABLE rozo_events DROP COLUMN IF EXISTS derive_generation;

ALTER TABLE cctp_events DROP COLUMN IF EXISTS derive_generation;

ALTER TABLE defindex_flows DROP COLUMN IF EXISTS derive_generation;

ALTER TABLE blend_emissions       DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE blend_admin           DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE blend_backstop_events DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE blend_auctions        DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE blend_emitter_events  DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE blend_positions       DROP COLUMN IF EXISTS derive_generation;

ALTER TABLE aquarius_rewards_events DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE aquarius_liquidity      DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE aquarius_reserves       DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE aquarius_admin          DROP COLUMN IF EXISTS derive_generation;

ALTER TABLE phoenix_liquidity    DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE phoenix_stake_events DROP COLUMN IF EXISTS derive_generation;

ALTER TABLE credit_events      DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE credit_settlements DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE credit_statements  DROP COLUMN IF EXISTS derive_generation;
ALTER TABLE credit_positions   DROP COLUMN IF EXISTS derive_generation;

COMMIT;
