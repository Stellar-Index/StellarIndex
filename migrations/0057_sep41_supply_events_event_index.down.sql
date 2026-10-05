-- Revert 0057: drop event_index (+ event_kind) from the sep41_supply_events PK.
-- Best-effort: if a re-derive under the new PK added rows that collide once
-- event_index / event_kind are removed, the narrowed PK re-add fails on
-- duplicates — expected (the wider PK exists precisely because those rows are
-- distinct). Decompress first.

-- Refuse before decompressing anything: up added the wider key because
-- events share the old one, so ADD PRIMARY KEY would fail mid-down (#1161).
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM sep41_supply_events GROUP BY contract_id, ledger, tx_hash, op_index, observed_at HAVING count(*) > 1) THEN
    RAISE EXCEPTION '0057_sep41_supply_events_event_index.down.sql: sep41_supply_events holds rows sharing the restored primary key (contract_id, ledger, tx_hash, op_index, observed_at) — down-migrating with data present is LOUD, not silent (#1161). Resolve the duplicates explicitly first.';
  END IF;
END $$;

SELECT decompress_chunk(c, true) FROM show_chunks('sep41_supply_events') c;
ALTER TABLE sep41_supply_events DROP CONSTRAINT sep41_supply_events_pkey;
ALTER TABLE sep41_supply_events ADD CONSTRAINT sep41_supply_events_pkey
    PRIMARY KEY (contract_id, ledger, tx_hash, op_index, observed_at);
ALTER TABLE sep41_supply_events DROP COLUMN IF EXISTS event_index;
