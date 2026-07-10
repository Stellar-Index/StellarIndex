-- 0101 down — remove the ROADMAP #11 call-tree position columns.

DROP INDEX IF EXISTS soroswap_router_swaps_call_kind_ts_idx;
ALTER TABLE soroswap_router_swaps DROP CONSTRAINT IF EXISTS soroswap_router_swaps_call_kind_check;
ALTER TABLE soroswap_router_swaps DROP COLUMN IF EXISTS call_kind;
ALTER TABLE soroswap_router_swaps DROP COLUMN IF EXISTS call_depth;
ALTER TABLE soroswap_router_swaps DROP COLUMN IF EXISTS call_path;
