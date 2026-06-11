-- Revert 0060: drop event_index from the phoenix_liquidity +
-- phoenix_stake_events PKs.
-- Best-effort: a re-derive under the wider PK may have added same-(op,action)
-- siblings that collide once event_index is removed; the narrowed PK re-add then
-- fails on duplicates — expected. Decompress first.
SELECT decompress_chunk(c, true) FROM show_chunks('phoenix_liquidity') c;
SELECT decompress_chunk(c, true) FROM show_chunks('phoenix_stake_events') c;

ALTER TABLE phoenix_liquidity DROP CONSTRAINT phoenix_liquidity_pkey;
ALTER TABLE phoenix_liquidity ADD CONSTRAINT phoenix_liquidity_pkey
    PRIMARY KEY (ledger_close_time, pool, ledger, tx_hash, op_index, action);
ALTER TABLE phoenix_liquidity DROP COLUMN IF EXISTS event_index;

ALTER TABLE phoenix_stake_events DROP CONSTRAINT phoenix_stake_events_pkey;
ALTER TABLE phoenix_stake_events ADD CONSTRAINT phoenix_stake_events_pkey
    PRIMARY KEY (ledger_close_time, stake_contract, ledger, tx_hash, op_index, action);
ALTER TABLE phoenix_stake_events DROP COLUMN IF EXISTS event_index;
