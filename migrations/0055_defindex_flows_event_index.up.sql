-- 0055: defindex_flows — add event_index to the PK.
--
-- defindex_flows keyed on (…, op_index, layer) with no per-event discriminator,
-- so when one operation emits multiple flows of the same (contract, layer) they
-- collide and all but one are dropped. The completeness verifier showed a
-- persistent Δ≈70 that OPTIMIZE (dup-cleanup) and re-derivation could not close
-- — confirming a real same-(contract,op,layer) collision, not dup-noise.
-- event_index (the contract event's index within the tx) is the unique
-- discriminator — same fix as blend (0053/0054).
--
-- Existing rows get event_index=0; the operator DELETEs blend... defindex_flows
-- and re-derives it with the new sink. The PK is added on the emptied table
-- (existing event_index=0 rows that differ only by layer would otherwise
-- collide on ADD — see the 0054 rollout note), so the operational order is:
-- decompress → ADD COLUMN → DROP old PK → DELETE → ADD new PK → re-derive.
--
-- For a fresh database the table is empty, so this file's ADD-PK succeeds
-- directly; on a populated host, DELETE before the ADD (operator runbook).

SELECT decompress_chunk(c, true) FROM show_chunks('defindex_flows') c;

ALTER TABLE defindex_flows ADD COLUMN IF NOT EXISTS event_index integer NOT NULL DEFAULT 0;
ALTER TABLE defindex_flows DROP CONSTRAINT defindex_flows_pkey;
ALTER TABLE defindex_flows ADD CONSTRAINT defindex_flows_pkey
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash, op_index, layer, event_index);
