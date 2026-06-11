-- 0057: sep41_supply_events — add event_index to the PK (F-1324).
--
-- The PK was (contract_id, ledger, tx_hash, op_index, observed_at) — no
-- per-event discriminator. A single Soroban operation can emit MORE THAN ONE
-- supply-affecting event (a contract that mints to several recipients, or
-- folds a burn + a clawback into one call): they share (contract_id, ledger,
-- tx_hash, op_index) and the same observed_at (one ledger close), so the coarse
-- PK collapsed them to one row via ON CONFLICT DO NOTHING — the same coarse-PK
-- data-loss class fixed for blend (0053/0054) / defindex (0055) /
-- soroswap-router (0056). event_index (the contract event's index within its
-- tx — events.Event.EventIndex) is the unique discriminator; event_kind also
-- drags in so a mint and a burn at the same event_index slot of a future WASM
-- can't collide.
--
-- Existing rows get event_index=0 (column default). The watched-set has been
-- restricted (SEP-41 observer not enabled on r1 per CLAUDE.md) so the table is
-- effectively empty; the ADD-PK succeeds directly. On a populated host, DELETE
-- the rows before the ADD-PK and re-derive with the event_index-aware sink (the
-- same operator order as 0055 — see the rollout runbook). STOP the indexer
-- before this migration and deploy the event_index sink before restarting, so
-- the new 7-col ON CONFLICT target matches the new PK.
--
-- TimescaleDB blocks constraint changes on compressed chunks; decompress first.

SELECT decompress_chunk(c, true) FROM show_chunks('sep41_supply_events') c;

ALTER TABLE sep41_supply_events ADD COLUMN IF NOT EXISTS event_index integer NOT NULL DEFAULT 0;
ALTER TABLE sep41_supply_events DROP CONSTRAINT sep41_supply_events_pkey;
ALTER TABLE sep41_supply_events ADD CONSTRAINT sep41_supply_events_pkey
    PRIMARY KEY (contract_id, ledger, tx_hash, op_index, observed_at, event_kind, event_index);
