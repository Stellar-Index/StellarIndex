-- Add the missing TimescaleDB compression POLICIES on served-tier
-- hypertables that have compression ELIGIBILITY set but no policy to
-- act on it. Capacity reclaim (~0.2-0.4 TiB of never-compressed aged
-- chunks — master-plan §0b §2).
--
-- Background: at creation, most served-tier hypertables set
--   ALTER TABLE x SET (timescaledb.compress, compress_segmentby=..., ...)
-- which only makes a table ELIGIBLE for compression — it does NOT
-- compress anything. Compression happens when a policy
-- (add_compression_policy) ages chunks past compress_after. `trades`
-- (0001) and `oracle_updates` (0003) got that policy; ~19 other
-- eligible tables never did, so their aged chunks sit uncompressed
-- forever. This script closes that gap. The 7-day interval matches the
-- house convention (12 of the existing policies use INTERVAL '7 days').
--
-- WHY THIS IS NOT A NUMBERED MIGRATION (ordering matters):
-- numbered migrations run in Phase C, BEFORE the Phase D (D4)
-- projector-replay backfills historical rows into these same
-- served-tier tables. Activating a compression policy before that
-- backfill would force D4's historical writes down the
-- insert-into-compressed path (decompress/restage) on the go-live long
-- pole. Run this AFTER D4 completes, when the tables are settled — then
-- the 7-day frontier is pure reclaim with no write interaction.
--   Sequence: D4 → cleanup → THIS → verify pool free space dropped.
--
-- Idempotent: if_not_exists => true, so re-running is a no-op for any
-- table that already has a policy (and safe to run after adding more
-- eligible tables — just extend the list).
--
-- NOT INCLUDED (need a compression-eligibility decision first — they
-- have NO `timescaledb.compress` set, so add_compression_policy would
-- error): api_usage_events (0027), usage_daily (0071). Enabling
-- compression on those needs a deliberate compress_segmentby choice;
-- tracked in the master plan, out of scope for this pure-policy script.
--
-- Operator usage (post-D4):
--   PGPASSWORD=$(cat /etc/stellarindex/postgres-password.txt) \
--   psql -h 127.0.0.1 -U stellarindex -d stellarindex \
--        -v ON_ERROR_STOP=1 -f scripts/ops/add-missing-compression-policies.sql
--
-- Verify afterward (chunks should begin compressing on the next policy
-- run; force-check one immediately if impatient):
--   SELECT hypertable_name, count(*) FILTER (WHERE is_compressed) AS compressed,
--          count(*) AS total
--   FROM timescaledb_information.chunks
--   GROUP BY 1 ORDER BY 1;

\set ON_ERROR_STOP on

DO $$
DECLARE
    -- Every served-tier hypertable that is compression-ELIGIBLE
    -- (SET timescaledb.compress at creation) but has no policy.
    tbl text;
    tables text[] := ARRAY[
        'account_observations',        -- 0010
        'aggregator_exposures',        -- 0025
        'blend_backstop_events',       -- 0063
        'cctp_events',                 -- 0038
        'claimable_observations',      -- 0012
        'classic_asset_stats_5m',      -- 0024
        'decoder_stats_5m',            -- 0020
        'defindex_flows',              -- 0050
        'divergence_observations',     -- 0019
        'freeze_events',               -- 0018
        'lp_reserve_observations',     -- 0013
        'price_source_contributions',  -- 0026
        'rozo_events',                 -- 0039
        'sac_balance_observations',    -- 0014
        'sdex_offer_events',           -- 0026
        'sep41_supply_events',         -- 0015
        'soroswap_router_swaps',       -- 0049
        'trustline_observations',      -- 0011
        'tvl_observations'             -- 0021
    ];
BEGIN
    FOREACH tbl IN ARRAY tables LOOP
        -- if_not_exists keeps this idempotent; a table missing/without
        -- compression eligibility would raise and ON_ERROR_STOP aborts,
        -- which is the intended fail-closed behavior (don't silently
        -- skip a table that should have been compressible).
        PERFORM add_compression_policy(tbl, compress_after => INTERVAL '7 days', if_not_exists => true);
        RAISE NOTICE 'compression policy ensured on % (compress_after 7 days)', tbl;
    END LOOP;
END $$;
