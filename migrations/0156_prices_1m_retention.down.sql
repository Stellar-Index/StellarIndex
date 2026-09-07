-- 0156 down — detach the `prices_1m` retention policy.
--
-- The up ships the policy DISABLED, so on an un-armed database this
-- down removes a job that has never dropped anything.
--
-- If it WAS armed, removing the policy stops further drops from this
-- moment on. It does NOT bring back minute buckets already dropped:
-- chunk removal is not transactional with the job that scheduled it,
-- and golang-migrate cannot un-drop a chunk.
--
-- Those buckets are recoverable, because raw `trades` is retained
-- forever (migration 0031) — but ONLY through the FORCED refresh,
-- run in slices, after this down has removed the policy:
--
--   CALL refresh_continuous_aggregate(
--          'prices_1m',
--          '<start>'::timestamptz, '<end>'::timestamptz,
--          force => true);
--
-- Without `force => true` the call finds no invalidation entries for
-- a retention-dropped range, reports the view already up-to-date,
-- writes nothing and exits 0.
--
-- ⚠ The TWAP views are built ON `prices_1m`'s materialization
-- hypertable, so an armed run left invalidation entries against them.
-- Refresh them WINDOWED and only after the rebuild above:
--
--   CALL refresh_continuous_aggregate(
--          'twap_1h',
--          '<start>'::timestamptz, '<end>'::timestamptz,
--          force => true);
--
-- Never the NULL-start form migrations 0081 / 0126 / 0147 document
-- (DO NOT RUN: CALL refresh_continuous_aggregate('twap_1h', NULL,
-- now());) — post-drop it processes the whole invalidation set
-- against an emptied `prices_1m` and DELETES the TWAP history.
--
-- See 0156_prices_1m_retention.up.sql for the full reasoning.

BEGIN;

SELECT remove_retention_policy('prices_1m', if_exists => true);

COMMIT;
