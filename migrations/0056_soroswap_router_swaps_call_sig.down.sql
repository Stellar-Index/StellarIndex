-- Revert 0056: drop call_sig from the soroswap_router_swaps PK.
SELECT decompress_chunk(c, true) FROM show_chunks('soroswap_router_swaps') c;
ALTER TABLE soroswap_router_swaps DROP CONSTRAINT soroswap_router_swaps_pkey;
ALTER TABLE soroswap_router_swaps ADD CONSTRAINT soroswap_router_swaps_pkey
    PRIMARY KEY (ledger_close_time, ledger, tx_hash, op_index);
ALTER TABLE soroswap_router_swaps DROP COLUMN IF EXISTS call_sig;
