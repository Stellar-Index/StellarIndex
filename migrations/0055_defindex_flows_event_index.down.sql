-- Revert 0055: drop event_index from the defindex_flows PK.

-- Refuse before decompressing anything: up added the wider key because
-- events share the old one, so ADD PRIMARY KEY would fail mid-down (#1161).
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM defindex_flows GROUP BY ledger_close_time, contract_id, ledger, tx_hash, op_index, layer HAVING count(*) > 1) THEN
    RAISE EXCEPTION '0055_defindex_flows_event_index.down.sql: defindex_flows holds rows sharing the restored primary key (ledger_close_time, contract_id, ledger, tx_hash, op_index, layer) — down-migrating with data present is LOUD, not silent (#1161). Resolve the duplicates explicitly first.';
  END IF;
END $$;

SELECT decompress_chunk(c, true) FROM show_chunks('defindex_flows') c;
ALTER TABLE defindex_flows DROP CONSTRAINT defindex_flows_pkey;
ALTER TABLE defindex_flows ADD CONSTRAINT defindex_flows_pkey
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash, op_index, layer);
ALTER TABLE defindex_flows DROP COLUMN IF EXISTS event_index;
