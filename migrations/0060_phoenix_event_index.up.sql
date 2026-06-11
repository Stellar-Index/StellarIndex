-- 0060: phoenix_liquidity + phoenix_stake_events — add event_index to the PKs
-- (F-1324).
--
-- Both PKs were (ledger_close_time, pool|stake_contract, ledger, tx_hash,
-- op_index, action) — no per-event discriminator. Phoenix reassembles one
-- logical action from N per-field events (provide=5, withdraw=4, bond/unbond=3)
-- sharing one (ledger, tx_hash, op_index). But a single op can carry MORE THAN
-- ONE logical action of the same kind: an auto-rebalance flow that provides
-- twice, or a batched stake op bonding twice, emits two complete reassemblies
-- that share (…, op_index, action) and the same ledger_close_time — so the
-- coarse PK collapsed them to one row via ON CONFLICT DO NOTHING. The swap path
-- already fans op_index by the first field-event's index (FanoutOpIndex); the
-- liquidity / stake paths used the raw op_index and so had no per-event
-- discriminator.
--
-- event_index is the in-op index of the FIRST field-event of the multi-event
-- reassembly (the buffer emits-and-clears each completed action before the next,
-- so each action's first-field index is distinct within the op) — the same
-- discriminator the swap path uses, and the same coarse-PK fix as blend
-- (0053/0054) / defindex (0055) / soroswap-router (0056) / comet (0059).
--
-- Existing rows get event_index=0 (column default). On a populated host they are
-- one-per-(op,action) so the ADD-PK succeeds directly; a re-derive then adds the
-- previously-dropped siblings. STOP the indexer before this migration and deploy
-- the event_index sink before restarting so the new 7-col ON CONFLICT targets
-- match the new PKs.
--
-- TimescaleDB blocks constraint changes on compressed chunks; decompress first.

SELECT decompress_chunk(c, true) FROM show_chunks('phoenix_liquidity') c;
SELECT decompress_chunk(c, true) FROM show_chunks('phoenix_stake_events') c;

ALTER TABLE phoenix_liquidity ADD COLUMN IF NOT EXISTS event_index integer NOT NULL DEFAULT 0;
ALTER TABLE phoenix_liquidity DROP CONSTRAINT phoenix_liquidity_pkey;
ALTER TABLE phoenix_liquidity ADD CONSTRAINT phoenix_liquidity_pkey
    PRIMARY KEY (ledger_close_time, pool, ledger, tx_hash, op_index, action, event_index);

ALTER TABLE phoenix_stake_events ADD COLUMN IF NOT EXISTS event_index integer NOT NULL DEFAULT 0;
ALTER TABLE phoenix_stake_events DROP CONSTRAINT phoenix_stake_events_pkey;
ALTER TABLE phoenix_stake_events ADD CONSTRAINT phoenix_stake_events_pkey
    PRIMARY KEY (ledger_close_time, stake_contract, ledger, tx_hash, op_index, action, event_index);
