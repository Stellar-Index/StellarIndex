-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1). It creates no object — the payload is ALTERs, and a FRESH
-- host's deploy/clickhouse/tier1_schema.sql already declares every index
-- below inside its CREATE TABLE.
-- Scope markers are enforced by scripts/ci/lint-ch-apply-scope.sh; the
-- fresh-host apply set is declared in
-- configs/ansible/roles/archival-node/tasks/08-clickhouse.yml.
--
-- The ADD INDEX path for the tier1 skip indexes that had none. Against a
-- table that predates an index, tier1_schema.sql's `CREATE TABLE IF NOT
-- EXISTS` is a no-op, and `MATERIALIZE INDEX` fails on an index the table
-- does not have — so without this file an existing host could not acquire
-- them at all. Each definition is byte-identical to tier1_schema.sql's;
-- internal/storage/clickhouse/skip_index_retrofit_test.go fails when a tier1
-- index has no matching ADD INDEX in some deploy/clickhouse artifact.
-- The four indexes not listed here are retrofitted by their own feature's
-- file (transactions_fee_bump.sql, ledger_entry_changes_key_xdr_index_fp.sql,
-- account_creators_rollup.sql, account_sponsors_rollup.sql).
--
-- Step 1 is metadata-only and idempotent (IF NOT EXISTS): every NEW part is
-- indexed on insert from then on, and a host that already has an index is
-- untouched. Old-binary-safe — no reader requires an index to be correct,
-- only to be fast.

-- ── Step 1: declare the indexes (instant) ────────────────────────────────────
ALTER TABLE stellar.transactions
    ADD INDEX IF NOT EXISTS idx_tx_hash tx_hash TYPE bloom_filter(0.01) GRANULARITY 1,
    ADD INDEX IF NOT EXISTS idx_tx_source source_account TYPE bloom_filter(0.01) GRANULARITY 1;

ALTER TABLE stellar.operations
    ADD INDEX IF NOT EXISTS idx_op_source source_account TYPE bloom_filter(0.01) GRANULARITY 1;

ALTER TABLE stellar.contract_events
    ADD INDEX IF NOT EXISTS idx_contract_id contract_id TYPE bloom_filter(0.01) GRANULARITY 1,
    ADD INDEX IF NOT EXISTS idx_ce_close_time close_time TYPE minmax GRANULARITY 1;

ALTER TABLE stellar.ledger_entry_changes
    ADD INDEX IF NOT EXISTS idx_lec_account_id account_id TYPE bloom_filter(0.01) GRANULARITY 1,
    ADD INDEX IF NOT EXISTS idx_lec_asset asset TYPE bloom_filter(0.01) GRANULARITY 1;

ALTER TABLE stellar.ledger_entries_current
    ADD INDEX IF NOT EXISTS idx_lecur_account_id account_id TYPE bloom_filter(0.01) GRANULARITY 1,
    ADD INDEX IF NOT EXISTS idx_lecur_asset asset TYPE bloom_filter(0.01) GRANULARITY 1;

ALTER TABLE stellar.account_movements
    ADD INDEX IF NOT EXISTS idx_cb_balance_id JSONExtractString(attributes, 'balance_id') TYPE bloom_filter(0.01) GRANULARITY 4;

-- ── Step 2: verify — expect 10 rows ──────────────────────────────────────────
-- SELECT table, name, type_full, granularity FROM system.data_skipping_indices
--  WHERE database = 'stellar'
--    AND name IN ('idx_tx_hash','idx_tx_source','idx_op_source',
--                 'idx_contract_id','idx_ce_close_time',
--                 'idx_lec_account_id','idx_lec_asset',
--                 'idx_lecur_account_id','idx_lecur_asset','idx_cb_balance_id')
--  ORDER BY table, name;

-- ── Step 3: materialize history, only for an index Step 1 just ADDED ─────────
-- Parts written before Step 1 are not indexed. MATERIALIZE is a heavy
-- mutation (it reads the column for every part), so run it per partition,
-- NEWEST first, one at a time, checking `df` against the 500 GiB floor
-- between each and letting each drain before the next — the same procedure
-- as ledger_entry_changes_key_xdr_index_fp.sql Step 3:
--
--   ALTER TABLE stellar.<table>
--       MATERIALIZE INDEX <index> IN PARTITION '{partition}';
--
--   SELECT partition_id, parts_to_do, is_done, latest_fail_reason
--   FROM system.mutations
--   WHERE database = 'stellar' AND table = '<table>' AND NOT is_done;
--
-- Idempotent and resumable: re-materializing a done partition is a cheap
-- no-op. An index that already existed before Step 1 needs no Step 3.
