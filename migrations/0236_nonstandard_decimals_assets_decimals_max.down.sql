-- 0236 down — drop the decimals ceiling; 0093's lower-bound CHECK remains.

BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE nonstandard_decimals_assets
    DROP CONSTRAINT IF EXISTS nonstandard_decimals_assets_decimals_max;

COMMIT;
