-- 0235 down — drop the exactly-one current-key CHECK; 0204's at-least-one
-- CHECK remains. The repaired rows are not restored: their dropped raw copy
-- was never read.

BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE customer_webhooks
    DROP CONSTRAINT IF EXISTS customer_webhooks_one_current_key;

COMMIT;
