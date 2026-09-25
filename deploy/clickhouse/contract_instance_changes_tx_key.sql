-- si-apply-scope: operator
-- si-cutover-object: stellar.contract_instance_changes_v2
-- si-cutover-object: stellar.contract_instance_changes_v2_mv
--
-- NOT applied by any bootstrap. Operator migration for an EXISTING
-- deployment (r1) whose stellar.contract_instance_changes was created under
-- ORDER BY (contract_hash, ledger_seq, change_index). A FRESH host does not
-- need it: tier1_schema.sql (and its mirror,
-- deploy/clickhouse/contract_instance_changes.sql) already declares the
-- tx-keyed table. The objects named above are cut-over halves — they exist
-- only for the duration of this migration, which ends by renaming v2 onto
-- the canonical name and dropping the old table.
--
-- THE DEFECT: change_index is a per-TRANSACTION counter
-- (internal/storage/clickhouse/extract_entry_changes.go), so two
-- transactions in one ledger writing the same contract's instance entry
-- collide on (contract_hash, ledger_seq, change_index) and the
-- ReplacingMergeTree keeps ONE of the two writes. A same-ledger upgrade
-- could vanish from /v1/contracts/{id}/code-history, and the survivor was
-- whichever insert came last, not the ledger-final executable. The new key
-- (contract_hash, ledger_seq, tx_hash, change_index) is the change's
-- identity; intra_ledger_seq is carried for the READ order. Full rationale
-- in the table's header in contract_instance_changes.sql.
--
-- WHY A SIDE TABLE: ClickHouse cannot re-key a MergeTree in place (MODIFY
-- ORDER BY only appends columns added by the same ALTER), and the rows the
-- old key already merged away are gone — they have to be re-derived from
-- ledger_entry_changes whatever the key. The old table keeps serving while
-- v2 fills; the cut-over is milliseconds of DDL.
--
-- BINARY / SCHEMA ORDER: either may land first. The explorer reader probes
-- the table for tx_hash + intra_ledger_seq and, against the old shape, reads
-- in the old change_index order instead of failing. ch-instance-backfill's
-- INSERT names the new columns, so against the OLD canonical table it errors
-- (unknown column) rather than writing — run it with
-- -table contract_instance_changes_v2 until Step 4 is done.
--
-- ***Heavy op.*** Step 2 re-reads every instance write in
-- ledger_entry_changes. Windowed, under run-heavy-job.sh, off-peak,
-- serialized with the other heavy jobs.

-- ── Step 0: preconditions (read-only) ──────────────────────────────────────
--   Needs the migration? Old key reads 'contract_hash, ledger_seq,
--   change_index'; 'contract_hash, ledger_seq, tx_hash, change_index' means
--   it is already done — stop.
--     SELECT sorting_key FROM system.tables
--      WHERE database = 'stellar' AND name = 'contract_instance_changes';
--   The source must carry intra_ledger_seq (the Step-1 MV selects it; the
--   CREATE fails loudly without it — see
--   ledger_entries_current_intra_ledger_seq.sql Step 0). Expect 1:
--     SELECT count() FROM system.columns WHERE database = 'stellar'
--        AND table = 'ledger_entry_changes' AND name = 'intra_ledger_seq';

-- ── Step 1: create v2 + its MV. The MV captures every ledger_entry_changes
-- insert from this moment, so Step 2 only has to cover history. Both
-- statements are the canonical DDL with the v2 names — a unit test
-- (contract_instance_changes_key_test.go) pins them in lockstep. ──────────

CREATE TABLE IF NOT EXISTS stellar.contract_instance_changes_v2
(
    contract_hash FixedString(64),  -- lower-hex 32-byte contract id
    ledger_seq    UInt32,
    tx_hash       String DEFAULT '', -- '' on snapshot/seed rows
    change_index  UInt32,           -- per-TRANSACTION position
    intra_ledger_seq UInt32 DEFAULT 0, -- per-LEDGER walk position
    close_time    DateTime('UTC'),
    is_sac        UInt8,            -- executable type: 0 = wasm, 1 = SAC
    wasm_hash     String,           -- lower-hex, '' when is_sac = 1
    ingested_at   DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
ORDER BY (contract_hash, ledger_seq, tx_hash, change_index);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.contract_instance_changes_v2_mv
TO stellar.contract_instance_changes_v2 AS
SELECT
    lower(hex(substring(tryBase64Decode(key_xdr), 9, 32)))  AS contract_hash,
    ledger_seq,
    tx_hash,
    change_index,
    intra_ledger_seq,
    close_time,
    toUInt8(substring(tryBase64Decode(entry_xdr), 61, 4) = unhex('00000001')) AS is_sac,
    if(substring(tryBase64Decode(entry_xdr), 61, 4) = unhex('00000000'),
       lower(hex(substring(tryBase64Decode(entry_xdr), 65, 32))), '')         AS wasm_hash
FROM stellar.ledger_entry_changes
WHERE entry_type = 'contract_data'
  AND length(key_xdr) = 64
  AND substring(tryBase64Decode(key_xdr), 1, 8) = unhex('0000000600000001')
  AND substring(tryBase64Decode(key_xdr), 41, 4) = unhex('00000014')
  AND entry_xdr != ''
  AND substring(tryBase64Decode(entry_xdr), 57, 4) = unhex('00000013');

-- ── Step 2: backfill v2 to genesis (a binary that has the -table flag).
-- -to defaults to the lake tip at start; the Step-1 MV already holds
-- everything after it, and overlap collapses in the RMT (same key):
--
--   /usr/local/sbin/run-heavy-job.sh instance-changes-v2-backfill \
--     /usr/local/bin/stellarindex-ops ch-instance-backfill \
--     -ch-addr 127.0.0.1:9300 -table contract_instance_changes_v2 \
--     -from 2 -window 2000000 -write
--
-- Resume an interrupted run with the -from it printed.

-- ── Step 3: verify v2 before cutting over ──────────────────────────────────
--   Every contract the old table knows, v2 knows (expect 0):
--     SELECT uniqExact(contract_hash) FROM stellar.contract_instance_changes
--      WHERE contract_hash NOT IN
--            (SELECT contract_hash FROM stellar.contract_instance_changes_v2);
--   v2 keeps what the old key merged, so v2 >= old:
--     SELECT (SELECT count() FROM stellar.contract_instance_changes FINAL) AS old,
--            (SELECT count() FROM stellar.contract_instance_changes_v2 FINAL) AS v2;
--   The defect's census — (contract, ledger, change_index) groups holding
--   more than one transaction's write, each of which the old table had
--   collapsed to one row:
--     SELECT count() FROM (
--       SELECT contract_hash, ledger_seq, change_index
--         FROM stellar.contract_instance_changes_v2 FINAL
--        GROUP BY contract_hash, ledger_seq, change_index
--       HAVING uniqExact(tx_hash) > 1);

-- ── Step 4: cut over. Drop BOTH MVs first — a renamed table does not carry
-- its MV's stored target along, and an MV aimed at a vanished name fails the
-- source INSERT (i.e. blocks ingest). Note the tip, then:
--
--   SELECT max(ledger_seq) FROM stellar.ledgers;           -- call it T
--   DROP VIEW IF EXISTS stellar.contract_instance_changes_mv;
--   DROP VIEW IF EXISTS stellar.contract_instance_changes_v2_mv;
--   RENAME TABLE stellar.contract_instance_changes    TO stellar.contract_instance_changes_old,
--                stellar.contract_instance_changes_v2 TO stellar.contract_instance_changes;
--
-- Recreate the canonical MV by applying
-- deploy/clickhouse/contract_instance_changes.sql (its CREATE TABLE is now a
-- no-op; its CREATE MATERIALIZED VIEW targets the renamed v2). Then close the
-- DDL gap — idempotent, so overlap is harmless:
--
--   /usr/local/bin/stellarindex-ops ch-instance-backfill \
--     -ch-addr 127.0.0.1:9300 -from <T - 1000> -write
--
-- Restart stellarindex-api: its key-shape probe latches the old-shape
-- verdict for the process lifetime, and until restarted it keeps reading in
-- the change_index order (no worse than before, but not the fix).

-- ── Step 5: after a settling period, DROP TABLE
-- stellar.contract_instance_changes_old SYNC.
--
-- ── ROLLBACK ────────────────────────────────────────────────────────────────
-- Before Step 4: DROP VIEW stellar.contract_instance_changes_v2_mv, then
-- DROP TABLE stellar.contract_instance_changes_v2. After Step 4 the previous
-- binary still reads the new table (it names only columns v2 kept); to
-- restore the old TABLE, drop the canonical MV, reverse the RENAME, and
-- recreate the MV from the previous release's contract_instance_changes.sql.
