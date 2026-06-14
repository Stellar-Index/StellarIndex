-- 0062 up — widen the chunk_time_interval of the high-volume hypertables
-- that were created with a 1-day interval.
--
-- Why: a 1-day interval on a hot append table accrues thousands of chunks
-- (trades reached 3445), and every INSERT ... ON CONFLICT walks all chunks
-- for the unique check → the insert rate collapses (~6/s observed) and
-- max_locks_per_transaction had to be bumped 64→256→4096 on r1
-- (audit-2026-06-14 A15; ADR-0034 served-tier). The r1 fix was operational
-- (merge_chunks() out-of-band), so the MIGRATION SET still encoded the broken
-- 1-day interval — a fresh archival-node bring-up (R2/R3/DR) would re-accrue
-- the same lock pressure as the table fills. This migration moves the encoded
-- default to 7 days (matching the dominant interval across the schema and the
-- operationally-validated ~6.5-day post-merge chunk width on trades).
--
-- set_chunk_time_interval only affects NEWLY-created chunks; existing chunks
-- keep their width. So on r1 (already merged) this just sizes future chunks
-- correctly; on a fresh install every chunk is 7 days from the start.
--
-- Forward-only in spirit (the down is a documented no-op): reverting to 1 day
-- would re-arm the exact lock-pressure pathology this fixes.

SELECT set_chunk_time_interval('trades',               INTERVAL '7 days');
SELECT set_chunk_time_interval('soroban_events',       INTERVAL '7 days');
SELECT set_chunk_time_interval('blend_auctions',       INTERVAL '7 days');
SELECT set_chunk_time_interval('phoenix_liquidity',    INTERVAL '7 days');
SELECT set_chunk_time_interval('phoenix_stake_events', INTERVAL '7 days');
