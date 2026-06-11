-- Revert 0058: drop event_index (+ event_kind) from the blend_auctions PK.
-- Best-effort: a re-derive under the wider PK may have added rows that collide
-- once event_index / event_kind are removed; the narrowed PK re-add then fails
-- on duplicates — expected (the wider PK exists because those rows are
-- distinct). Decompress first.
SELECT decompress_chunk(c, true) FROM show_chunks('blend_auctions') c;
ALTER TABLE blend_auctions DROP CONSTRAINT blend_auctions_pkey;
ALTER TABLE blend_auctions ADD CONSTRAINT blend_auctions_pkey
    PRIMARY KEY (ledger, tx_hash, op_index, ts);
ALTER TABLE blend_auctions DROP COLUMN IF EXISTS event_index;
