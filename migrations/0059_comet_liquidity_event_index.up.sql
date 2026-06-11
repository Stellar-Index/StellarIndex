-- 0059: comet_liquidity — add event_index to the PK (F-1324).
--
-- The PK was (ledger_close_time, contract_id, ledger, tx_hash, op_index,
-- event_kind, token). (event_kind, token) distinguishes per-token liquidity
-- changes, but NOT two events of the SAME (kind, token) in one op: a single
-- operation can emit two join_pool events for the SAME token (a contract that
-- tops up the same reserve twice, or an aggregator routing through the pool
-- twice in one call). Those share (ledger, tx_hash, op_index, event_kind, token)
-- and the same ledger_close_time, so the coarse PK collapsed them to one row via
-- ON CONFLICT DO NOTHING. The comet SWAP path already fans op_index by
-- event_index (canonical.FanoutOpIndex) to avoid exactly this; the liquidity
-- path used the raw OperationIndex and so had no per-event discriminator.
--
-- event_index (the contract event's index within its tx — events.Event.EventIndex)
-- is the unique discriminator — same fix as blend (0053/0054) / defindex (0055) /
-- soroswap-router (0056). It's added AFTER token in the key so the existing
-- per-token uniqueness is preserved and only previously-dropped same-(kind,token)
-- siblings are added on a re-derive.
--
-- Existing rows get event_index=0 (column default). On a populated host they are
-- one-per-(kind,token,op) so the ADD-PK succeeds directly; a re-derive then adds
-- the previously-dropped siblings (no DELETE needed for the existing rows). STOP
-- the indexer before this migration and deploy the event_index sink before
-- restarting so the new 8-col ON CONFLICT target matches the new PK.
--
-- TimescaleDB blocks constraint changes on compressed chunks; decompress first.

SELECT decompress_chunk(c, true) FROM show_chunks('comet_liquidity') c;

ALTER TABLE comet_liquidity ADD COLUMN IF NOT EXISTS event_index integer NOT NULL DEFAULT 0;
ALTER TABLE comet_liquidity DROP CONSTRAINT comet_liquidity_pkey;
ALTER TABLE comet_liquidity ADD CONSTRAINT comet_liquidity_pkey
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_kind, token, event_index);
