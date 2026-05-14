-- 0031 down — restore the original 90-day / 30-day retention
-- policies. Reverses the 2026-05-14 "store all raw trades forever"
-- change. Use only if disk pressure forces a rollback.

BEGIN;

SELECT add_retention_policy('trades',     INTERVAL '90 days', if_not_exists => true);
SELECT add_retention_policy('prices_1m',  INTERVAL '30 days', if_not_exists => true);
SELECT add_retention_policy('prices_15m', INTERVAL '30 days', if_not_exists => true);

COMMIT;
