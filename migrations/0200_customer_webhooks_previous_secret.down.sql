-- 0200 down — drop the rotation overlap columns. Webhooks keep their
-- current key (secret_hash); any outgoing key still in its overlap window
-- stops signing immediately.

BEGIN;

ALTER TABLE customer_webhooks
    DROP CONSTRAINT IF EXISTS customer_webhooks_previous_secret_pair;

ALTER TABLE customer_webhooks
    DROP COLUMN IF EXISTS previous_secret_expires_at,
    DROP COLUMN IF EXISTS previous_secret;

COMMIT;
