-- 0199 up — rotate a webhook signing key in place, with an overlap window.
--
-- WHAT IS WRONG TODAY
--
-- A customer can only rotate a webhook's signing key by deleting and
-- re-creating the webhook (GH #665). The delete cascades to
-- webhook_deliveries, so every queued or retrying delivery and the
-- delivery log go with it, and the new key takes effect the instant the
-- new row exists: a receiver still holding the old key rejects every
-- delivery until it is redeployed.
--
-- WHAT THIS MIGRATION CHANGES
--
-- Adds the outgoing key and the time it stops being used. Rotation is an
-- UPDATE of the same row: secret_hash takes the new key, previous_secret
-- the old one, and until previous_secret_expires_at the delivery worker
-- signs with both (the old key in the X-StellarIndex-Signature-Previous
-- headers). The row, its queue and its log are untouched. The CHECK keeps
-- the pair set or cleared together.
--
-- RULE 9. Two nullable columns and a CHECK every existing row satisfies
-- (both NULL): catalog-only, no rewrite. The previous binary neither
-- reads nor writes them and signs with secret_hash alone, as it does now.

BEGIN;

ALTER TABLE customer_webhooks
    ADD COLUMN IF NOT EXISTS previous_secret bytea,
    ADD COLUMN IF NOT EXISTS previous_secret_expires_at timestamptz;

ALTER TABLE customer_webhooks
    DROP CONSTRAINT IF EXISTS customer_webhooks_previous_secret_pair;
ALTER TABLE customer_webhooks
    ADD CONSTRAINT customer_webhooks_previous_secret_pair  -- migration-compat:ok constrains only the two columns added above; old binaries never write them, so both stay NULL
    CHECK ((previous_secret IS NULL) = (previous_secret_expires_at IS NULL));

COMMENT ON COLUMN customer_webhooks.previous_secret IS
    'The RAW HMAC key secret_hash held before the last rotation (not a hash, like secret_hash). Deliveries are also signed with it until previous_secret_expires_at.';
COMMENT ON COLUMN customer_webhooks.previous_secret_expires_at IS
    'End of the rotation overlap: deliveries stop carrying the previous_secret signature after this instant.';

COMMIT;
