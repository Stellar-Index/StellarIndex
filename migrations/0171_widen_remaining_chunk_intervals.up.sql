-- 0171 up — widen the chunk_time_interval of every hypertable 0062 missed.
--
-- 0062 moved trades, soroban_events, blend_auctions, phoenix_liquidity and
-- phoenix_stake_events from 1-day to 7-day chunks, because a 1-day interval
-- on an append table accrues thousands of chunks and every ON CONFLICT
-- insert walks all of them for the unique check (trades reached 3445 chunks
-- and max_locks_per_transaction had to go 64 -> 4096 on r1). It left nine
-- siblings created with the same narrow interval, so a fresh archival-node
-- bring-up still re-accrues that lock pressure on them:
--
--   oracle_updates          1 day   (0003)
--   api_usage_events        1 day   (0027)
--   soroswap_skim_events    1 day   (0043)
--   blend_positions         1 day   (0045)
--   blend_emissions         1 day   (0045)
--   blend_admin             1 day   (0045)
--   sep41_transfers         1 day   (0047)
--   blend_backstop_events   1 day   (0063)
--   aquarius_rewards_events 3 days  (0099)
--
-- After this migration no public hypertable is narrower than 7 days, and
-- internal/storage/timescale/chunk_interval_floor_test.go fails the build
-- if a later migration creates one.
--
-- The new interval only applies to chunks created from now on; existing
-- chunks keep their width, so this rewrites nothing and takes no data lock.
--
-- Consequences, checked per table:
--   * Compression (oracle_updates, sep41_transfers, the blend tables,
--     soroswap_skim_events, blend_backstop_events): a chunk is compressed
--     once its whole range is older than compress_after, so rows now stay
--     uncompressed for up to 7 more days. Late-arriving writes land in an
--     uncompressed chunk no less often than before.
--   * Retention (api_usage_events, 12 months): a chunk is dropped once its
--     whole range is past drop_after, so a row now lives up to ~12 months +
--     7 days instead of + 1 day. usage_daily already runs the same 12-month
--     policy over 90-day chunks (0167).
--
-- Forward-only: the down is a documented no-op, as 0062's is.

SELECT set_chunk_time_interval('oracle_updates',          INTERVAL '7 days');
SELECT set_chunk_time_interval('api_usage_events',        INTERVAL '7 days');
SELECT set_chunk_time_interval('soroswap_skim_events',    INTERVAL '7 days');
SELECT set_chunk_time_interval('blend_positions',         INTERVAL '7 days');
SELECT set_chunk_time_interval('blend_emissions',         INTERVAL '7 days');
SELECT set_chunk_time_interval('blend_admin',             INTERVAL '7 days');
SELECT set_chunk_time_interval('sep41_transfers',         INTERVAL '7 days');
SELECT set_chunk_time_interval('blend_backstop_events',   INTERVAL '7 days');
SELECT set_chunk_time_interval('aquarius_rewards_events', INTERVAL '7 days');
