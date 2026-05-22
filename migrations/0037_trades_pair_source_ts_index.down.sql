-- 0037 down — drop the LatestTradePerSource composite index (#30).
--
-- On a live node prefer DROP INDEX CONCURRENTLY by hand; the
-- in-transaction DROP INDEX below takes a brief AccessExclusive lock
-- on the index only, which is acceptable for a rollback window.

DROP INDEX IF EXISTS trades_pair_source_ts_idx;
