-- Revert 0057: drop event_index (+ event_kind) from the sep41_supply_events PK.
-- Best-effort: if a re-derive under the new PK added rows that collide once
-- event_index / event_kind are removed, the narrowed PK re-add fails on
-- duplicates — expected (the wider PK exists precisely because those rows are
-- distinct). Decompress first.
SELECT decompress_chunk(c, true) FROM show_chunks('sep41_supply_events') c;
ALTER TABLE sep41_supply_events DROP CONSTRAINT sep41_supply_events_pkey;
ALTER TABLE sep41_supply_events ADD CONSTRAINT sep41_supply_events_pkey
    PRIMARY KEY (contract_id, ledger, tx_hash, op_index, observed_at);
ALTER TABLE sep41_supply_events DROP COLUMN IF EXISTS event_index;
