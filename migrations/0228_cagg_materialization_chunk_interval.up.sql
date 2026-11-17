-- 0228 up — 7-day chunks for the trades-derived continuous aggregates'
-- materialization hypertables.
--
-- Invariant: the materialization hypertable of every trades-derived
-- continuous aggregate (prices_*, twap_*, pools_per_source_1h,
-- trade_sums_1m, dex_volume_by_pair_1d, source_volume_1h: thirteen views)
-- creates chunks 7 days wide, the width trades itself has used since 0062.
--
-- A CREATE MATERIALIZED VIEW ... WITH (timescaledb.continuous) sizes its
-- materialization chunks at 10x the source hypertable's interval: 70 days
-- over trades, wider again for twap_* over prices_1m. 0187 recreated ten of
-- these views, 0217 created trade_sums_1m, and 0064 and 0068 created
-- dex_volume_by_pair_1d and source_volume_1h, so each refresh rewrites one
-- multi-month chunk holding tens of millions of rows, which is the /v1/price
-- p95 tail that a hand-run set_chunk_time_interval fixed on r1 before 0187
-- silently undid it. The setting lives on the hypertable, so storing it
-- here makes every host and every fresh install agree.
--
-- set_chunk_time_interval only affects chunks created from now on; an open
-- wide chunk keeps its range until it closes (at most ~70 days). No data is
-- blanked or rewritten, so there is no REQUIRED-FOLLOWUP. A catalog-only
-- change: the previous binary never reads chunk geometry (rule 9).
--
-- Each call names the view, not its _materialized_hypertable_N (that number
-- differs per database): TimescaleDB resolves a continuous aggregate to its
-- materialization hypertable itself, and a missing view fails the cast.

BEGIN;

SET LOCAL lock_timeout = '5s';

SELECT set_chunk_time_interval('prices_1m', INTERVAL '7 days');
SELECT set_chunk_time_interval('prices_15m', INTERVAL '7 days');
SELECT set_chunk_time_interval('prices_1h', INTERVAL '7 days');
SELECT set_chunk_time_interval('prices_4h', INTERVAL '7 days');
SELECT set_chunk_time_interval('prices_1d', INTERVAL '7 days');
SELECT set_chunk_time_interval('prices_1w', INTERVAL '7 days');
SELECT set_chunk_time_interval('prices_1mo', INTERVAL '7 days');
SELECT set_chunk_time_interval('twap_1h', INTERVAL '7 days');
SELECT set_chunk_time_interval('twap_1d', INTERVAL '7 days');
SELECT set_chunk_time_interval('pools_per_source_1h', INTERVAL '7 days');
SELECT set_chunk_time_interval('trade_sums_1m', INTERVAL '7 days');
SELECT set_chunk_time_interval('dex_volume_by_pair_1d', INTERVAL '7 days');
SELECT set_chunk_time_interval('source_volume_1h', INTERVAL '7 days');

COMMIT;
