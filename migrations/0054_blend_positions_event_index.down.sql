-- Revert 0054: restore the (asset, user_address) discriminator PK.
SELECT decompress_chunk(c, true) FROM show_chunks('blend_positions') c;
ALTER TABLE blend_positions DROP CONSTRAINT blend_positions_pkey;
ALTER TABLE blend_positions ADD CONSTRAINT blend_positions_pkey
    PRIMARY KEY (pool, ledger, tx_hash, op_index, event_kind, asset, user_address, ledger_close_time);
ALTER TABLE blend_positions DROP COLUMN IF EXISTS event_index;
