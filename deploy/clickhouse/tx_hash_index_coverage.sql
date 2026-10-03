-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1). A FRESH host's deploy/clickhouse/tier1_schema.sql already
-- declares the table below.
--
-- Backfill-completion marker for stellar.tx_hash_index. The reader serves
-- GET /v1/tx/{hash} from the index only while a row exists here; otherwise it
-- takes the bloom scan.
--
-- *** ORDERING *** apply this, then either run
--   stellarindex-ops ch-txindex-backfill -full -write
-- (writes the row on completion) or, if the index is already known to be
-- backfilled genesis→tip, insert it directly:
--   INSERT INTO stellar.tx_hash_index_coverage (covered_from, covered_to)
--   VALUES (2, <lake tip>);
-- Restart the api afterwards: a probe that found the table absent latches
-- for the process lifetime.
--
-- Whenever tx_hash_index is dropped and recreated, TRUNCATE
-- stellar.tx_hash_index_coverage too: a stale marker over an emptied index
-- makes every historical hash lookup answer not-found.

CREATE TABLE IF NOT EXISTS stellar.tx_hash_index_coverage
(
    covered_from UInt32,
    covered_to   UInt32,
    completed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY (covered_from, covered_to);
