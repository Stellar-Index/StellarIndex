-- Revert 0056: drop call_sig from the soroswap_router_swaps PK.

-- Refuse before decompressing anything: up added the wider key because
-- events share the old one, so ADD PRIMARY KEY would fail mid-down (#1161).
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM soroswap_router_swaps GROUP BY ledger_close_time, ledger, tx_hash, op_index HAVING count(*) > 1) THEN
    RAISE EXCEPTION '0056_soroswap_router_swaps_call_sig.down.sql: soroswap_router_swaps holds rows sharing the restored primary key (ledger_close_time, ledger, tx_hash, op_index) — down-migrating with data present is LOUD, not silent (#1161). Resolve the duplicates explicitly first.';
  END IF;
END $$;

SELECT decompress_chunk(c, true) FROM show_chunks('soroswap_router_swaps') c;
ALTER TABLE soroswap_router_swaps DROP CONSTRAINT soroswap_router_swaps_pkey;
ALTER TABLE soroswap_router_swaps ADD CONSTRAINT soroswap_router_swaps_pkey
    PRIMARY KEY (ledger_close_time, ledger, tx_hash, op_index);
ALTER TABLE soroswap_router_swaps DROP COLUMN IF EXISTS call_sig;
