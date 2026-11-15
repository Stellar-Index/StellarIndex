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
-- ***Heavy op.*** run-heavy-job.sh, one partition at a time; the loop stops at the first partition that did not run
-- to success and prints COMPLETE only when every one did. Re-run it whole after an abort. The MV rows for
-- the partition that held T0 overlap the backfill; that only raises cnt.
--
--   (
--     case "$T0" in ''|*[^0-9]*|0*)
--       echo "contract_event_shapes backfill: T0='$T0' is not a ledger number - aborting, NOTHING was backfilled" >&2; exit 1 ;;
--     esac
--     CES_MARKS=$(mktemp -d) || exit 1
--     CES_DONE=0; CES_WANT=0
--     ces_job() {
--       CES_MARK="$CES_MARKS/$1-$2.ok"
--       /usr/local/sbin/run-heavy-job.sh "event-$1-$2" bash -c 'clickhouse-client --port 9300 -q "$1" && : > "$2"' ces-job "$3" "$CES_MARK" </dev/null \
--         || { if [ $? -eq 75 ]; then echo "contract_event_shapes backfill: $1 partition $2 DID NOT RUN (the wrapper exited 75 - a lock was held (this job's, or the host-wide one another heavy job holds) and it did not run) - aborting, the table is INCOMPLETE from partition $2 up"; else echo "contract_event_shapes backfill: $1 partition $2 FAILED - aborting, the table is INCOMPLETE from partition $2 up"; fi >&2; exit 1; }
--       [ -e "$CES_MARK" ] \
--         || { echo "contract_event_shapes backfill: $1 partition $2 DID NOT RUN (the wrapper exited 0 with no success marker) - aborting, the table is INCOMPLETE from partition $2 up" >&2; exit 1; }
--       CES_DONE=$((CES_DONE + 1))
--     }
--     for P in $(seq 50 "$((T0 / 1000000))"); do
--       CES_WANT=$((CES_WANT + 1))
--       ces_job shapes "$P" "
--         INSERT INTO stellar.contract_event_shapes
--         SELECT intDiv(ledger_seq, 1000000) AS part, contract_id, topic_0_sym,
--                if(topic_0_sym = '', topics_xdr[1], '') AS t0,
--                if(topic_0_sym = '', topics_xdr[2], '') AS t1,
--                if(topic_0_sym = '', length(topics_xdr), 0) AS tn,
--                count(), min(ledger_seq), max(ledger_seq)
--         FROM stellar.contract_events
--         WHERE ledger_seq >= $((P * 1000000)) AND ledger_seq < $(((P + 1) * 1000000))
--           AND ledger_seq < $T0
--         GROUP BY part, contract_id, topic_0_sym, t0, t1, tn
--         SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--     done
--     echo "contract_event_shapes backfill: $CES_DONE of $CES_WANT jobs ran to success"
--     [ "$CES_WANT" -gt 0 ] && [ "$CES_DONE" -eq "$CES_WANT" ] \
--       || { echo "contract_event_shapes backfill: NOT every job ran (or T0=$T0 yields no window) - aborting" >&2; exit 1; }
--     rm -rf "$CES_MARKS"
--   ) && echo "contract_event_shapes backfill: COMPLETE - every partition 50..$((T0 / 1000000)) covered"
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
-- Only after Step 2 printed COMPLETE: a partition the wrapper refused (exit 75) or that failed aborts the loop without
-- that line, and a marker written over a skipped partition makes the reader trust shapes that are not there.
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
