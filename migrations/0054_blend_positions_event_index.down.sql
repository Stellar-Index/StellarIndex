-- Revert 0054: restore the (asset, user_address) discriminator PK.

-- Refuse before decompressing anything: up added the wider key because
-- events share the old one, so ADD PRIMARY KEY would fail mid-down (#1161).
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM blend_positions GROUP BY pool, ledger, tx_hash, op_index, event_kind, asset, user_address, ledger_close_time HAVING count(*) > 1) THEN
    RAISE EXCEPTION '0054_blend_positions_event_index.down.sql: blend_positions holds rows sharing the restored primary key (pool, ledger, tx_hash, op_index, event_kind, asset, user_address, ledger_close_time) — down-migrating with data present is LOUD, not silent (#1161). Resolve the duplicates explicitly first.';
  END IF;
END $$;

SELECT decompress_chunk(c, true) FROM show_chunks('blend_positions') c;
ALTER TABLE blend_positions DROP CONSTRAINT blend_positions_pkey;
ALTER TABLE blend_positions ADD CONSTRAINT blend_positions_pkey
    PRIMARY KEY (pool, ledger, tx_hash, op_index, event_kind, asset, user_address, ledger_close_time);
ALTER TABLE blend_positions DROP COLUMN IF EXISTS event_index;
