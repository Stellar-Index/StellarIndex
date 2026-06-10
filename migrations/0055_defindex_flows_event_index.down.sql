-- Revert 0055: drop event_index from the defindex_flows PK.
SELECT decompress_chunk(c, true) FROM show_chunks('defindex_flows') c;
ALTER TABLE defindex_flows DROP CONSTRAINT defindex_flows_pkey;
ALTER TABLE defindex_flows ADD CONSTRAINT defindex_flows_pkey
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash, op_index, layer);
ALTER TABLE defindex_flows DROP COLUMN IF EXISTS event_index;
