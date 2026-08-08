-- contract_instance_changes — per-contract instance-executable timeline
-- index for the explorer's code-history + wasm reads (open-fixes
-- inventory #26 items 3 + wasm; route sweeps 2026-07-29 → 2026-08-09:
-- /v1/contracts/{id}/code-history was the LAST persistent 503 class).
--
-- WHY: stellar.ledger_entry_changes is ORDER BY (ledger_seq, …), so the
-- "instance entry for contract X" predicate (key_xdr IN (…)) is
-- scan-shaped over the whole changes history — 8s+ cold and 503 under
-- the explorer budget for any non-prewarmed contract. Both
-- ContractCodeHistory and the ContractWasm hash resolution pay it.
--
-- THE INDEX: one narrow row per captured instance-entry WRITE, keyed
-- (contract, ledger, change_index), carrying just the decoded verdict:
-- SAC or wasm + which hash. The wire layout makes the MV extraction a
-- pair of fixed-offset substrings (byte-verified 2026-08-09 against
-- go-stellar-sdk marshalling, all three shapes):
--
--   key_xdr   (48 bytes): [1-4]=LedgerKey type contract_data(6)
--                         [5-8]=SCAddress type contract(1)
--                         [9-40]=contract id  [41-44]=SCVal type
--                         ScvLedgerKeyContractInstance(0x14)
--                         [45-48]=durability
--   entry_xdr (var):      [57-60]=val type ScvContractInstance(0x13)
--                         [61-64]=executable type (0=wasm 1=SAC)
--                         [65-96]=wasm hash (wasm only)
--
-- Instance-STORAGE writes rewrite the same key (audit-2026-07-23 C-F1),
-- so a busy contract contributes many rows — but each is ~90 bytes vs
-- the multi-KB entry_xdr, and readers collapse to distinct executables.
--
-- REPLAY-SAFE: no counts; identical (contract, ledger, change_index)
-- keys re-inserted by re-derives/overlapping backfills collapse in the
-- RMT (the migration-0059 double-count class does not apply).
--
-- OPERATOR CONTRACT (same as contract_active_ledgers): presence +
-- non-empty = readers TRUST it, including per-contract emptiness
-- ("instance never captured"). Apply this DDL, then IMMEDIATELY run the
-- windowed historical backfill to genesis — serialized with other heavy
-- jobs:
--
--   /usr/local/sbin/run-heavy-job.sh instance-changes-backfill \
--     /usr/local/bin/stellarindex-ops ch-instance-backfill \
--     -ch-addr 127.0.0.1:9300 -from 2 -window 2000000
--
-- Do NOT leave the table applied-but-unbackfilled on a lake with
-- history: cold contracts would resolve "no wasm" / truncated upgrade
-- timelines stamped as complete.

CREATE TABLE IF NOT EXISTS stellar.contract_instance_changes
(
    contract_hash FixedString(64),  -- lower-hex 32-byte contract id
    ledger_seq    UInt32,
    change_index  UInt32,
    close_time    DateTime('UTC'),
    is_sac        UInt8,            -- executable type: 0 = wasm, 1 = SAC
    wasm_hash     String,           -- lower-hex, '' when is_sac = 1
    ingested_at   DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
ORDER BY (contract_hash, ledger_seq, change_index);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.contract_instance_changes_mv
TO stellar.contract_instance_changes AS
SELECT
    lower(hex(substring(tryBase64Decode(key_xdr), 9, 32)))  AS contract_hash,
    ledger_seq,
    change_index,
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
