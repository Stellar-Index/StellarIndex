-- Revert 0058: drop event_index (+ event_kind) from the blend_auctions PK.
-- Best-effort: a re-derive under the wider PK may have added rows that collide
-- once event_index / event_kind are removed; the narrowed PK re-add then fails
-- on duplicates — expected (the wider PK exists because those rows are
-- distinct). Decompress first.

-- Refuse before decompressing anything: up added the wider key because
-- events share the old one, so ADD PRIMARY KEY would fail mid-down (#1161).
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM blend_auctions GROUP BY ledger, tx_hash, op_index, ts HAVING count(*) > 1) THEN
    RAISE EXCEPTION '0058_blend_auctions_event_index.down.sql: blend_auctions holds rows sharing the restored primary key (ledger, tx_hash, op_index, ts) — down-migrating with data present is LOUD, not silent (#1161). Resolve the duplicates explicitly first.';
  END IF;
END $$;

SELECT decompress_chunk(c, true) FROM show_chunks('blend_auctions') c;
ALTER TABLE blend_auctions DROP CONSTRAINT blend_auctions_pkey;
ALTER TABLE blend_auctions ADD CONSTRAINT blend_auctions_pkey
    PRIMARY KEY (ledger, tx_hash, op_index, ts);
ALTER TABLE blend_auctions DROP COLUMN IF EXISTS event_index;
