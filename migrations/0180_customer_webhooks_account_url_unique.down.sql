-- 0180 down — drop the (account_id, url) uniqueness. No rows change.

BEGIN;

DROP INDEX IF EXISTS customer_webhooks_account_url_key;

COMMIT;
