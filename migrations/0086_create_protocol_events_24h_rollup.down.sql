-- 0086 down — drop the protocol_events_24h rollup.
--
-- Correctness-safe: with the table absent, CountRecentEventsBySource
-- returns an empty map and /v1/protocols renders events_24h = 0. The
-- pre-0086 live census path is not auto-restored (the reader was moved
-- to the rollup), so a full rollback also needs the code reverted; on
-- its own this only blanks the events_24h column.
DROP TABLE IF EXISTS protocol_events_24h;
