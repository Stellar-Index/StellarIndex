-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1, testnet, futurenet) for the DDL below plus its backfill. A
-- FRESH host gets these objects from deploy/clickhouse/tier1_schema.sql, which
-- declares each of them identically (scripts/ci/lint-ch-apply-scope.sh rule 5).
--
-- account_op_types: (account -> op key, op_type) for every op an account's
-- history shows. Source arm: the MV below, from stellar.operations. Participant
-- arm: the CH sink, beside stellar.operation_participants, only for ops whose tx
-- is visible to the participant (successful, or sourced by it) — the same rule
-- as the reader's visibleTxPredicate. A participant is never the op source, so
-- the arms never share a row and count() FINAL gives today's per-type totals.
--
-- WHY: AccountOperationTypeCounts joined account keys into stellar.operations
-- (24.8B rows) to read op_type, one granule per op: 22.5 s / 360M rows for the
-- USDC issuer, >2 min for a 2.5M-op account, so /v1/accounts/{g}/activity
-- 503'd cold for every heavy account. This is a PK-prefix range read.
--
-- *** ORDERING ***
--   Step 1 (DDL, instant) on EVERY host before the binary that writes the
--     participant arm: the sink's schema check refuses to start without it.
--   Step 2 deploy that binary; record T0 = max(ledger_seq) FROM stellar.ledgers
--     once ingest is running it.
--   Step 3 windowed backfill [2, T0 + 10000): overlap with live writes is
--     idempotent under the RMT.
--   Step 4 verify, Step 5 write the coverage marker and restart the api. The
--     reader keeps the old join until the marker exists.
--
-- Re-deriving stellar.operation_participants for a range (ch-participant-
-- backfill) must re-run Step 3's participant arm for the same range, or the
-- two tables drift. Whenever account_op_types is dropped or rebuilt, TRUNCATE
-- stellar.account_op_types_coverage too: a stale marker over an emptied table
-- serves zero counts.

-- ── Step 1: tables + source MV ──────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS stellar.account_op_types
(
    account    String,
    ledger_seq UInt32,
    tx_index   UInt32,
    op_index   UInt32,
    op_type    LowCardinality(String)
)
ENGINE = ReplacingMergeTree
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY (account, ledger_seq, tx_index, op_index);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.account_op_types_source_mv
TO stellar.account_op_types AS
SELECT source_account AS account, ledger_seq, tx_index, op_index, op_type
FROM stellar.operations
WHERE source_account != '';

CREATE TABLE IF NOT EXISTS stellar.account_op_types_coverage
(
    covered_from UInt32,
    covered_to   UInt32,
    completed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY (covered_from, covered_to);

-- ── Step 3: windowed historical backfill ────────────────────────────────────
-- ***Heavy op.*** run-heavy-job.sh, one window at a time. Duplicate source rows
-- (operations and transactions are RMTs holding rows twice) collapse in the
-- target RMT, so no dedup is needed on the read side. The participant arm keeps
-- the participants window on the JOIN's build side and pre-filters
-- transactions to its tx keys; drop W to 100000 if a dense window exceeds memory.
-- Measured on r1: the participant arm over a dense 10k-ledger window (60.0M) read
-- 4.7M participant rows, kept 3.3M visible, in 9.3 s; budget ~16 h for history.
--
--   W=200000
--   for S in $(seq 2 "$W" "$((T0 + 10000))"); do E=$((S + W))
--     /usr/local/sbin/run-heavy-job.sh "aot-source-$S" clickhouse-client --port 9300 -q "
--       INSERT INTO stellar.account_op_types (account, ledger_seq, tx_index, op_index, op_type)
--       SELECT source_account, ledger_seq, tx_index, op_index, op_type
--       FROM stellar.operations
--       WHERE source_account != '' AND ledger_seq >= $S AND ledger_seq < $E
--       SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--     /usr/local/sbin/run-heavy-job.sh "aot-participant-$S" clickhouse-client --port 9300 -q "
--       INSERT INTO stellar.account_op_types (account, ledger_seq, tx_index, op_index, op_type)
--       SELECT p.account, o.ledger_seq, o.tx_index, o.op_index, o.op_type
--       FROM stellar.operations AS o
--       INNER JOIN (
--         SELECT p.account, p.ledger_seq, p.tx_index, p.op_index
--         FROM (SELECT DISTINCT account, ledger_seq, tx_index, op_index
--               FROM stellar.operation_participants
--               WHERE ledger_seq >= $S AND ledger_seq < $E) AS p
--         INNER JOIN (
--           SELECT ledger_seq, tx_index, any(successful) AS ok, any(source_account) AS src
--           FROM stellar.transactions
--           WHERE ledger_seq >= $S AND ledger_seq < $E
--             AND (ledger_seq, tx_index) IN (
--                 SELECT ledger_seq, tx_index FROM stellar.operation_participants
--                 WHERE ledger_seq >= $S AND ledger_seq < $E)
--           GROUP BY ledger_seq, tx_index) AS t
--         ON p.ledger_seq = t.ledger_seq AND p.tx_index = t.tx_index
--         WHERE t.ok = 1 OR t.src = p.account) AS p
--       ON o.ledger_seq = p.ledger_seq AND o.tx_index = p.tx_index AND o.op_index = p.op_index
--       WHERE o.ledger_seq >= $S AND o.ledger_seq < $E
--       SETTINGS max_threads = 4, max_memory_usage = 8000000000, join_algorithm = 'grace_hash'"
--   done
--
-- ── Step 4: verify ──────────────────────────────────────────────────────────
-- For 20 accounts (include the USDC issuer and a multi-million-op account),
-- per-op-type counts from
--   SELECT op_type, count() FROM stellar.account_op_types FINAL
--   WHERE account = '<G>' GROUP BY op_type
--   SETTINGS do_not_merge_across_partitions_select_final = 1
-- must equal accountOpTypeCountsQuery's answer (explorer_reader.go) for the
-- same account. Expect every row equal.
--
-- ── Step 5: coverage marker ─────────────────────────────────────────────────
--   INSERT INTO stellar.account_op_types_coverage (covered_from, covered_to)
--   VALUES (2, <T0 + 10000>);
-- Then restart the api: a probe that found the marker absent latches for the
-- process lifetime.
--
-- ── ROLLBACK ────────────────────────────────────────────────────────────────
-- Roll the binaries back to a build that does not write or read the table
-- first (the sink refuses to start without it), then:
--   DROP TABLE IF EXISTS stellar.account_op_types_source_mv;
--   DROP TABLE IF EXISTS stellar.account_op_types_coverage;
--   DROP TABLE IF EXISTS stellar.account_op_types SYNC;
