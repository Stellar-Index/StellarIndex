-- Add the missing TimescaleDB compression POLICIES on served-tier
-- hypertables that have compression ELIGIBILITY set but no policy to act on
-- it (reclaims ~0.2-0.4 TiB of never-compressed aged chunks).
--
-- Background: at creation, most served-tier hypertables set
--   ALTER TABLE x SET (timescaledb.compress, compress_segmentby=..., ...)
-- which only makes a table ELIGIBLE for compression; it does NOT compress
-- anything. Chunks compress when a policy (add_compression_policy) ages them
-- past compress_after, and only `trades` (0001) and `oracle_updates` (0003)
-- got one. The 7-day interval matches the house convention.
--
-- WHY THIS IS NOT A NUMBERED MIGRATION (ordering matters):
-- numbered migrations run in Phase C, BEFORE the Phase D (D4) projector-replay
-- backfills historical rows into these served-tier tables. A compression
-- policy active before that backfill would force D4's historical writes down
-- the insert-into-compressed path (decompress/restage). Run this AFTER D4
-- completes, when the tables are settled; then the 7-day frontier is pure
-- reclaim with no write interaction.
--   Sequence: D4 → cleanup → THIS → verify pool free space dropped.
--
-- Idempotent: if_not_exists => true, so re-running is a no-op for any table
-- that already has a policy (extend the list to cover more tables).
--
-- NOT INCLUDED (no `timescaledb.compress` set, so add_compression_policy
-- would error): api_usage_events (0027), usage_daily (0071). Enabling
-- compression there needs a deliberate compress_segmentby choice.
--
-- NOT INCLUDED (segment-by is near-unique per row, so compression stores about
-- one row per segment and GROWS the chunk): claimable_observations (0012),
-- sac_balance_observations (0014), soroswap_router_swaps (0049),
-- trustline_observations (0011). Re-segment on the served predicate first.
--
-- Dropped by migration 0152: aggregator_exposures (0025),
-- classic_asset_stats_5m (0024) and tvl_observations (0021). A dropped table
-- MUST come out of the list below in the same change: `\set ON_ERROR_STOP on`
-- plus add_compression_policy on a missing table aborts the whole DO block,
-- so every table AFTER it silently never gets its policy.
--
-- Operator usage (post-D4):
--   PGPASSWORD=$(cat /etc/stellarindex/postgres-password.txt) \
--   psql -h 127.0.0.1 -U stellarindex -d stellarindex \
--        -v ON_ERROR_STOP=1 -f scripts/ops/add-missing-compression-policies.sql
--
-- Verify afterward (chunks begin compressing on the next policy run):
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
        'blend_backstop_events',       -- 0063
        'cctp_events',                 -- 0038
        'decoder_stats_5m',            -- 0020
        'defindex_flows',              -- 0050
        'divergence_observations',     -- 0019
        'freeze_events',               -- 0018
        'lp_reserve_observations',     -- 0013
        'price_source_contributions',  -- 0026
        'rozo_events',                 -- 0039
        'sdex_offer_events',           -- 0026
        'sep41_supply_events'          -- 0015
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
