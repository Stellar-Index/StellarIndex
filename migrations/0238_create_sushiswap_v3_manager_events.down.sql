-- Revert 0238: drop the sushiswap_v3_manager_events hypertable.
BEGIN;
DROP TABLE IF EXISTS sushiswap_v3_manager_events;
COMMIT;
