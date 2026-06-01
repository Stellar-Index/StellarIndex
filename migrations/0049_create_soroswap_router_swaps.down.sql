-- 0049 down — drop soroswap_router_swaps hypertable.

BEGIN;

DROP TABLE IF EXISTS soroswap_router_swaps;

COMMIT;
