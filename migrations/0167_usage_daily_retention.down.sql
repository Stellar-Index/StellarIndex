-- 0167 down — remove the `usage_daily` retention policy. Rows the
-- policy already dropped are not restored.

BEGIN;

SELECT remove_retention_policy('usage_daily', if_exists => true);

COMMIT;
