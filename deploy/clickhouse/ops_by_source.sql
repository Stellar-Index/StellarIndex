-- ops_by_source: slim sourced-history projection — (source_account →
-- ledger/tx/op keys), MV-maintained from BOTH stellar.operations and
-- stellar.transactions.
--
-- Why this table exists: the account-history readers' "sourced" arms
-- (explorer_reader.go accountTransactionsQuery / accountOperationsQuery
-- arm 1) resolved `source_account = ?` via the bloom skip-index over the
-- full 23B-row transactions / 34B-row operations tables. Granule-pruned
-- but still scan-shaped: MEASURED on r1 2026-07-30 for a 328-op account,
-- arm 1 = 6.17 s vs arm 2 (operation_participants, PK-prefixed) = 0.056 s
-- — 110×, and the reason /v1/accounts/{g}/transactions|operations sat in
-- the 8s 503 class under load. This projection is the same fix class as
-- stellar.ttl_live_until: extract the lookup ONCE per row at ingest into a
-- slim PK-ordered table, make the read a primary-index range.
--
-- ONE table serves BOTH readers via an op_index sentinel:
--   * rows from stellar.operations carry the OP-EFFECTIVE source (an op
--     can override its tx's source) with the real op_index → the
--     OPERATIONS arm reads them (op_index != sentinel);
--   * rows from stellar.transactions carry the TX source (fee payer) with
--     op_index = 4294967295 (the sentinel) → the TRANSACTIONS arm reads
--     DISTINCT (ledger_seq, tx_index) over ALL rows, so a tx is found
--     whether the account sourced the tx itself or any single op in it.
-- Keeping both streams preserves the readers' exact semantics: a tx
-- sourced by A whose every op overrides to another source is still A's;
-- an op sourced by B inside A's tx is still B's.
--
-- Engine: ReplacingMergeTree over the full sort key — rows are identity
-- facts (no versions to race); a backfill window re-run or MV/backfill
-- overlap dedups to one row. Readers keep their LIMIT 1 BY guards, so
-- un-merged duplicates never shorten a page.
--
-- Operator sequence (deploy order matters — the v0.21.7+ binary REFUSES
-- account-history reads until this table exists; no scan fallback):
--   Step 1 (table + MVs, instant) → Step 2 (windowed backfills, heavy)
--   → Step 3 (verify) → deploy the binary that reads it.

-- ── Step 1: table + both MVs ────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS stellar.ops_by_source
(
    source_account String,
    ledger_seq     UInt32,
    tx_index       UInt32,
    op_index       UInt32
)
ENGINE = ReplacingMergeTree
ORDER BY (source_account, ledger_seq, tx_index, op_index);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.ops_by_source_ops_mv
TO stellar.ops_by_source AS
SELECT source_account, ledger_seq, tx_index, op_index
FROM stellar.operations
WHERE source_account != '';

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.ops_by_source_tx_mv
TO stellar.ops_by_source AS
SELECT source_account, ledger_seq, tx_index, 4294967295 AS op_index
FROM stellar.transactions
WHERE source_account != '';

-- ── Step 2: windowed historical backfill ────────────────────────────────────
-- ***Heavy op.*** run-heavy-job.sh, ONE window at a time, 2M-ledger windows
-- (both source tables are PARTITION BY intDiv(ledger_seq, 1000000); the
-- selects are 4 narrow columns, no XDR decode — far lighter per-row than the
-- ttl backfill). Capture TIP at MV-creation time; overlapping the MV era is
-- idempotent under the RMT. Re-running any window is SAFE.
--
--   TIP=$(clickhouse-client --port 9300 -q "SELECT max(ledger_seq) FROM stellar.ledgers")
--   for W in $(seq 2 2000000 "$TIP"); do
--     /usr/local/sbin/run-heavy-job.sh "obs-backfill-ops-$W" clickhouse-client --port 9300 -q "
--       INSERT INTO stellar.ops_by_source
--       SELECT source_account, ledger_seq, tx_index, op_index
--       FROM stellar.operations
--       WHERE source_account != '' AND ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--       SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--     /usr/local/sbin/run-heavy-job.sh "obs-backfill-tx-$W" clickhouse-client --port 9300 -q "
--       INSERT INTO stellar.ops_by_source
--       SELECT source_account, ledger_seq, tx_index, 4294967295
--       FROM stellar.transactions
--       WHERE source_account != '' AND ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--       SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--   done
--
-- ── Step 3: verify ──────────────────────────────────────────────────────────
-- Spot-check N accounts: the projection's per-account key set must equal the
-- bloom-probe answer over the source tables (bounded per-account reads):
--
--   SELECT countIf(a != b) FROM (
--     SELECT
--       (SELECT countDistinct(ledger_seq, tx_index) FROM stellar.transactions WHERE source_account = t.sa) AS a,  -- countDistinct, NOT count(): raw counts include un-merged RMT duplicate parts (first verify run 2026-07-30 flagged 20/20 'mismatches' that were exactly this)
--       (SELECT countDistinct(ledger_seq, tx_index) FROM stellar.ops_by_source
--         WHERE source_account = t.sa AND op_index = 4294967295) AS b,
--       sa
--     FROM (SELECT DISTINCT source_account AS sa FROM stellar.transactions
--           WHERE ledger_seq > (SELECT max(ledger_seq) - 10000 FROM stellar.ledgers)
--           LIMIT 20) t)
--
-- Expect 0.
--
-- ── ROLLBACK ────────────────────────────────────────────────────────────────
-- Additive projection; nothing else writes or reads it. Pair a drop with
-- rolling the binaries back to a pre-projection build:
--   DROP TABLE IF EXISTS stellar.ops_by_source_ops_mv;
--   DROP TABLE IF EXISTS stellar.ops_by_source_tx_mv;
--   DROP TABLE IF EXISTS stellar.ops_by_source SYNC;
