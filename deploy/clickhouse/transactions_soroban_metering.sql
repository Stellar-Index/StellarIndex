-- stellar.transactions: add Soroban resource-metering columns (5.1-full Slice A).
--
-- Additive, DEFAULT 0 → metadata-only for existing parts (NO rewrite), O(1),
-- and old-binary-safe: an indexer that doesn't yet write these columns keeps
-- inserting fine (they default 0), and a reader that doesn't select them is
-- unaffected. Same class as
-- deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql's additive
-- pattern. FRESH deployments do NOT need this file — tier1_schema.sql's
-- canonical CREATE already carries these columns (`CREATE TABLE IF NOT EXISTS`
-- is a no-op against the existing r1 table).
--
-- *** DEPLOY ORDERING (REQUIRED) — apply this BEFORE the indexer binary ***
-- The new indexer's flushTxs INSERT names these columns explicitly. If that
-- binary ships before the ALTER lands, every transactions insert fails against
-- a table lacking the columns and INGEST HALTS (the "config-gated feature ships
-- dead on binary-only deploy" class, commit c82f3b0; a binary deploy does NOT
-- apply deploy/clickhouse/ per scripts/ci/config-apply-gate.sh). Sequence:
--   1. Apply this file on r1 (clickhouse-client < transactions_soroban_metering.sql).
--   2. Verify the columns exist (see the SELECT at the bottom).
--   3. THEN deploy the indexer + api binaries.
--
-- These are populated GO-FORWARD only. Historical Soroban txs read 0 — the
-- declared/charged values aren't recoverable from what's stored (they live in
-- the tx envelope/meta, which the lake doesn't persist); a backfill would be a
-- ch-rebuild over archived LCM (ExtractLedger, ADR-0043 Phase C), out of scope.

ALTER TABLE stellar.transactions
    ADD COLUMN IF NOT EXISTS soroban_instructions      UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS soroban_disk_read_bytes   UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS soroban_write_bytes       UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS soroban_read_entries      UInt16 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS soroban_write_entries     UInt16 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS soroban_resource_fee_bid  Int64  DEFAULT 0,
    ADD COLUMN IF NOT EXISTS soroban_nonrefundable_fee Int64  DEFAULT 0,
    ADD COLUMN IF NOT EXISTS soroban_refundable_fee    Int64  DEFAULT 0,
    ADD COLUMN IF NOT EXISTS soroban_rent_fee          Int64  DEFAULT 0;

-- Verify (step 2): expect 9 rows.
-- SELECT name, type FROM system.columns
--  WHERE database='stellar' AND table='transactions' AND name LIKE 'soroban_%'
--  ORDER BY name;
