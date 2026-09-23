-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1). A FRESH host's deploy/clickhouse/tier1_schema.sql already
-- declares every column, index and view below.
-- Scope markers are enforced by scripts/ci/lint-ch-apply-scope.sh; the
-- fresh-host apply set is declared in
-- configs/ansible/roles/archival-node/tasks/08-clickhouse.yml.
--
-- stellar.transactions: add the fee-bump outer layer, and index a fee bump's
-- inner hash in stellar.tx_hash_index.
--
-- The ALTER is additive with defaults → metadata-only for existing parts (NO
-- rewrite) and old-binary-safe, the same class as
-- transactions_soroban_metering.sql. ADD INDEX covers new parts only; the
-- skip index serves the bloom-scan fallback, and history has nothing in the
-- column to index.
--
-- *** DEPLOY ORDERING (REQUIRED) — apply this BEFORE the indexer and api ***
-- The new indexer's flushTxs INSERT and the new api's transaction reads name
-- these columns explicitly. A binary that ships before the ALTER fails every
-- transactions insert (INGEST HALTS) and every transaction read. Sequence:
--   1. Apply this file on r1 (clickhouse-client --multiquery < transactions_fee_bump.sql).
--   2. Verify (see the SELECTs at the bottom).
--   3. THEN deploy the indexer + api binaries.
--
-- Populated GO-FORWARD only. A historical fee bump (result_code 1 or -13)
-- reads ''/0 in these columns, and its inner hash stays unresolvable, until
-- its range is re-derived from archived LCM (ch-rebuild through
-- ExtractLedger). The MV indexes the re-derived rows' inner hashes; for a
-- recreated tx_hash_index, stellarindex-ops ch-txindex-backfill indexes both
-- hashes.

ALTER TABLE stellar.transactions
    ADD COLUMN IF NOT EXISTS inner_tx_hash     String DEFAULT '',
    ADD COLUMN IF NOT EXISTS fee_account       String DEFAULT '',
    ADD COLUMN IF NOT EXISTS fee_bump_fee      Int64  DEFAULT 0,
    ADD COLUMN IF NOT EXISTS inner_result_code Int32  DEFAULT 0,
    ADD INDEX IF NOT EXISTS idx_tx_inner_hash inner_tx_hash TYPE bloom_filter(0.01) GRANULARITY 1;

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.tx_hash_index_inner_mv
TO stellar.tx_hash_index AS
SELECT inner_tx_hash AS tx_hash, ledger_seq, tx_index FROM stellar.transactions
WHERE inner_tx_hash != '';

-- Verify (step 2): expect 4 rows, then 1 row.
-- SELECT name, type FROM system.columns
--  WHERE database='stellar' AND table='transactions'
--    AND name IN ('inner_tx_hash','fee_account','fee_bump_fee','inner_result_code')
--  ORDER BY name;
-- SELECT name FROM system.tables
--  WHERE database='stellar' AND name='tx_hash_index_inner_mv';
