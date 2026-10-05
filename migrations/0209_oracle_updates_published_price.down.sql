-- 0209 down — drop oracle_updates.published_price. The recorded integers are
-- lost from the served tier; the raw events in the lake still hold them.

BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE oracle_updates
    DROP COLUMN published_price;

COMMIT;
