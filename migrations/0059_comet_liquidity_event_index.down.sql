-- Revert 0059: drop event_index from the comet_liquidity PK.
-- Best-effort: a re-derive under the wider PK may have added same-(kind,token,op)
-- siblings that collide once event_index is removed; the narrowed PK re-add then
-- fails on duplicates — expected. Decompress first.

-- Refuse before decompressing anything: up added the wider key because
-- events share the old one, so ADD PRIMARY KEY would fail mid-down (#1161).
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM comet_liquidity GROUP BY ledger_close_time, contract_id, ledger, tx_hash, op_index, event_kind, token HAVING count(*) > 1) THEN
    RAISE EXCEPTION '0059_comet_liquidity_event_index.down.sql: comet_liquidity holds rows sharing the restored primary key (ledger_close_time, contract_id, ledger, tx_hash, op_index, event_kind, token) — down-migrating with data present is LOUD, not silent (#1161). Resolve the duplicates explicitly first.';
  END IF;
END $$;

SELECT decompress_chunk(c, true) FROM show_chunks('comet_liquidity') c;
ALTER TABLE comet_liquidity DROP CONSTRAINT comet_liquidity_pkey;
ALTER TABLE comet_liquidity ADD CONSTRAINT comet_liquidity_pkey
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_kind, token);
ALTER TABLE comet_liquidity DROP COLUMN IF EXISTS event_index;
