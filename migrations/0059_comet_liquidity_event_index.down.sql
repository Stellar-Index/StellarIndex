-- Revert 0059: drop event_index from the comet_liquidity PK.
-- Best-effort: a re-derive under the wider PK may have added same-(kind,token,op)
-- siblings that collide once event_index is removed; the narrowed PK re-add then
-- fails on duplicates — expected. Decompress first.
SELECT decompress_chunk(c, true) FROM show_chunks('comet_liquidity') c;
ALTER TABLE comet_liquidity DROP CONSTRAINT comet_liquidity_pkey;
ALTER TABLE comet_liquidity ADD CONSTRAINT comet_liquidity_pkey
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_kind, token);
ALTER TABLE comet_liquidity DROP COLUMN IF EXISTS event_index;
