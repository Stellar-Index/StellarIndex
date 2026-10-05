-- Revert 0053. Best-effort: if a re-derive under the new PK added rows that
-- collide once (asset, user_address) / event_index are removed, the narrowed
-- PK re-add will fail on duplicates — that is expected (the wider PK exists
-- precisely because those rows are distinct). Decompress first.

-- Refuse before decompressing anything: up added the wider key because
-- events share the old one, so ADD PRIMARY KEY would fail mid-down (#1161).
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM blend_positions GROUP BY pool, ledger, tx_hash, op_index, event_kind, ledger_close_time HAVING count(*) > 1) THEN
    RAISE EXCEPTION '0053_blend_pk_granularity.down.sql: blend_positions holds rows sharing the restored primary key (pool, ledger, tx_hash, op_index, event_kind, ledger_close_time) — down-migrating with data present is LOUD, not silent (#1161). Resolve the duplicates explicitly first.';
  END IF;
  IF EXISTS (SELECT 1 FROM blend_emissions GROUP BY pool, ledger, tx_hash, op_index, event_kind, ledger_close_time HAVING count(*) > 1) THEN
    RAISE EXCEPTION '0053_blend_pk_granularity.down.sql: blend_emissions holds rows sharing the restored primary key (pool, ledger, tx_hash, op_index, event_kind, ledger_close_time) — down-migrating with data present is LOUD, not silent (#1161). Resolve the duplicates explicitly first.';
  END IF;
  IF EXISTS (SELECT 1 FROM blend_admin GROUP BY contract_id, ledger, tx_hash, op_index, event_kind, ledger_close_time HAVING count(*) > 1) THEN
    RAISE EXCEPTION '0053_blend_pk_granularity.down.sql: blend_admin holds rows sharing the restored primary key (contract_id, ledger, tx_hash, op_index, event_kind, ledger_close_time) — down-migrating with data present is LOUD, not silent (#1161). Resolve the duplicates explicitly first.';
  END IF;
END $$;

SELECT decompress_chunk(c, true) FROM show_chunks('blend_positions') c;
SELECT decompress_chunk(c, true) FROM show_chunks('blend_emissions') c;
SELECT decompress_chunk(c, true) FROM show_chunks('blend_admin') c;

ALTER TABLE blend_positions DROP CONSTRAINT blend_positions_pkey;
ALTER TABLE blend_positions ADD CONSTRAINT blend_positions_pkey
    PRIMARY KEY (pool, ledger, tx_hash, op_index, event_kind, ledger_close_time);

ALTER TABLE blend_emissions DROP CONSTRAINT blend_emissions_pkey;
ALTER TABLE blend_emissions ADD CONSTRAINT blend_emissions_pkey
    PRIMARY KEY (pool, ledger, tx_hash, op_index, event_kind, ledger_close_time);
ALTER TABLE blend_emissions DROP COLUMN IF EXISTS event_index;

ALTER TABLE blend_admin DROP CONSTRAINT blend_admin_pkey;
ALTER TABLE blend_admin ADD CONSTRAINT blend_admin_pkey
    PRIMARY KEY (contract_id, ledger, tx_hash, op_index, event_kind, ledger_close_time);
ALTER TABLE blend_admin DROP COLUMN IF EXISTS event_index;
