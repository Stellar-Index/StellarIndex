-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1, testnet, futurenet) for the DDL below plus its backfill. A
-- FRESH host gets these objects from deploy/clickhouse/tier1_schema.sql,
-- which declares each of them identically.
-- Scope markers are enforced by scripts/ci/lint-ch-apply-scope.sh; the
-- fresh-host apply set is declared in
-- configs/ansible/roles/archival-node/tasks/08-clickhouse.yml.
--
-- contract_event_shapes: one row per distinct event shape per lake partition
-- (the recognition identity DistinctTopicShapes groups on: contract_id,
-- topic_0_sym and, when topic[0] is not a Symbol, topics_xdr[1], topics_xdr[2]
-- and arity), with its row count and first/last ledger.
--
-- WHY: the recognition audit (compute-completeness, ch-recognition) re-scanned
-- every Soroban-era partition of stellar.contract_events on each run. At the 2
-- threads boundedScanSettings allows, one 1M-ledger window took ~27 s for the
-- Symbol shapes and ~20 s for the non-Symbol ones (their topics_xdr reads touch
-- half the granules), so the scan ran 14-25 min. DistinctTopicShapes now reads
-- whole partitions from here and scans only the partial edge windows.
--
-- No code path deletes from stellar.contract_events, so min/max ledger and the
-- shape set are exact. cnt counts inserted rows, duplicates included, as the
-- raw count() over the ReplacingMergeTree does; a re-inserted window raises it.
--
-- *** ORDERING ***
--   Step 1 (DDL, instant): the MV captures every insert from here on.
--     Record T0 = max(ledger_seq) FROM stellar.contract_events right after.
--   Step 2 windowed backfill of partitions [F, intDiv(T0, 1000000)], where F
--     is the first partition DistinctTopicShapes will read (the Soroban era
--     starts in partition 50).
--   Step 3 verify, Step 4 write the coverage marker. The reader keeps the raw
--     scan until the marker exists.
--
-- Whenever contract_event_shapes is dropped or rebuilt, TRUNCATE
-- stellar.contract_event_shapes_coverage too: a marker over an emptied table
-- reports no shapes for the covered partitions.

-- ── Step 1: tables + MV ─────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS stellar.contract_event_shapes
(
    part        UInt32,
    contract_id String,
    topic_0_sym String,
    t0          String,
    t1          String,
    tn          UInt64,
    cnt         SimpleAggregateFunction(sum, UInt64),
    lo          SimpleAggregateFunction(min, UInt32),
    hi          SimpleAggregateFunction(max, UInt32)
)
ENGINE = AggregatingMergeTree
PARTITION BY part
ORDER BY (contract_id, topic_0_sym, t0, t1, tn);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.contract_event_shapes_mv
TO stellar.contract_event_shapes AS
SELECT
    intDiv(ledger_seq, 1000000) AS part,
    contract_id,
    topic_0_sym,
    if(topic_0_sym = '', topics_xdr[1], '') AS t0,
    if(topic_0_sym = '', topics_xdr[2], '') AS t1,
    if(topic_0_sym = '', length(topics_xdr), 0) AS tn,
    count() AS cnt,
    min(ledger_seq) AS lo,
    max(ledger_seq) AS hi
FROM stellar.contract_events
GROUP BY part, contract_id, topic_0_sym, t0, t1, tn;

CREATE TABLE IF NOT EXISTS stellar.contract_event_shapes_coverage
(
    covered_from UInt32,
    covered_to   UInt32,
    completed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY (covered_from, covered_to);

-- ── Step 2: windowed historical backfill ────────────────────────────────────
-- ***Heavy op.*** run-heavy-job.sh, one partition at a time. The MV rows for
-- the partition that held T0 overlap the backfill; that only raises cnt.
--
--   for P in $(seq 50 "$((T0 / 1000000))"); do
--     /usr/local/sbin/run-heavy-job.sh "event-shapes-$P" clickhouse-client --port 9300 -q "
--       INSERT INTO stellar.contract_event_shapes
--       SELECT intDiv(ledger_seq, 1000000) AS part, contract_id, topic_0_sym,
--              if(topic_0_sym = '', topics_xdr[1], '') AS t0,
--              if(topic_0_sym = '', topics_xdr[2], '') AS t1,
--              if(topic_0_sym = '', length(topics_xdr), 0) AS tn,
--              count(), min(ledger_seq), max(ledger_seq)
--       FROM stellar.contract_events
--       WHERE ledger_seq >= $((P * 1000000)) AND ledger_seq < $(((P + 1) * 1000000))
--         AND ledger_seq < $T0
--       GROUP BY part, contract_id, topic_0_sym, t0, t1, tn
--       SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--   done
--
-- The ledger_seq < T0 bound keeps the backfill off rows the MV already holds.
--
-- ── Step 3: verify ──────────────────────────────────────────────────────────
-- For three partitions (the first, a dense one, the last whole one), the shape
-- set and min/max ledger must equal the raw scan's:
--   SELECT contract_id, topic_0_sym, t0, t1, tn, min(lo), max(hi)
--   FROM stellar.contract_event_shapes WHERE part = <P>
--   GROUP BY contract_id, topic_0_sym, t0, t1, tn
-- against the same GROUP BY over stellar.contract_events for that partition,
-- compared with EXCEPT in both directions. Expect zero rows each way.
--
-- ── Step 4: coverage marker ─────────────────────────────────────────────────
--   INSERT INTO stellar.contract_event_shapes_coverage (covered_from, covered_to)
--   VALUES (<F * 1000000>, <T0>);
-- covered_from is the first ledger of the first backfilled partition: every
-- whole partition at or above it is read from contract_event_shapes.
--
-- ── ROLLBACK ────────────────────────────────────────────────────────────────
--   TRUNCATE TABLE stellar.contract_event_shapes_coverage;
--   DROP TABLE IF EXISTS stellar.contract_event_shapes_mv;
--   DROP TABLE IF EXISTS stellar.contract_event_shapes;
--   DROP TABLE IF EXISTS stellar.contract_event_shapes_coverage;
