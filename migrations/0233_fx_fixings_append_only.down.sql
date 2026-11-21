-- 0233 down — drop fx_fixings' append-only triggers and their function,
-- and clear the ingested_at column comment 0233 added. No data changes.

BEGIN;

SET LOCAL lock_timeout = '5s';

DROP TRIGGER IF EXISTS fx_fixings_no_truncate ON fx_fixings;
DROP TRIGGER IF EXISTS fx_fixings_append_only ON fx_fixings;
DROP FUNCTION IF EXISTS fx_fixings_append_only();

COMMENT ON COLUMN fx_fixings.ingested_at IS NULL;

COMMIT;
