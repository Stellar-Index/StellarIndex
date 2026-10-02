-- 0199 up — hold customer-webhook signing keys sealed at rest.
--
-- WHAT IS WRONG TODAY
--
-- customer_webhooks.secret_hash holds the RAW HMAC-SHA-256 signing key
-- (0151's comment says so). It cannot be one-way hashed — the receiver
-- verifies with the same shared secret — so anyone with SELECT on the
-- table, or a copy of a backup, can forge deliveries to every endpoint.
--
-- WHAT THIS MIGRATION CHANGES
--
-- Adds signing_key_sealed: the key sealed by the API with AES-256-GCM
-- under a key that lives only in the API's environment, bound to the
-- row's id. secret_hash becomes nullable so a sealed row carries no raw
-- copy; a CHECK keeps at least one of the two present. The API writes
-- sealed-only rows when its seal key is configured and seals existing
-- raw rows at startup (compare-and-swap per row).
--
-- RULE 9. migrate up runs before the binary swap. The previous binary
-- names secret_hash with a value on INSERT (passes the CHECK) and reads
-- only secret_hash, which is still populated for every row until the
-- new binary starts. Rolling the BINARY back after it has sealed rows
-- leaves those rows unreadable to the old worker (it terminally fails
-- their deliveries as "no_secret"), so roll back the binary only
-- together with a restore of the raw keys.
--
-- The ADD CONSTRAINT validates by scanning the table: one row per
-- registered webhook, capped per account.

BEGIN;

ALTER TABLE customer_webhooks
    ADD COLUMN IF NOT EXISTS signing_key_sealed bytea;

ALTER TABLE customer_webhooks
    ALTER COLUMN secret_hash DROP NOT NULL;

ALTER TABLE customer_webhooks
    DROP CONSTRAINT IF EXISTS customer_webhooks_signing_key_present;

ALTER TABLE customer_webhooks
    ADD CONSTRAINT customer_webhooks_signing_key_present  -- migration-compat:ok the old binary's only INSERT always names a non-NULL secret_hash, and every existing row has one (it was NOT NULL)
    CHECK (secret_hash IS NOT NULL OR signing_key_sealed IS NOT NULL);

COMMENT ON COLUMN customer_webhooks.signing_key_sealed IS
    'HMAC-SHA-256 signing key sealed by the API (version byte, then '
    'AES-256-GCM with the row id as associated data; key from the API '
    'env named by api.dashboard.webhook_seal_key_env). NULL only on a row '
    'still holding its key raw in secret_hash.';

COMMENT ON COLUMN customer_webhooks.secret_hash IS
    'MISNAMED legacy column. Holds the RAW HMAC-SHA-256 signing key, not a '
    'hash of one, for rows written without a seal key; NULL once the key '
    'is sealed into signing_key_sealed. Treat a non-NULL value as a '
    'credential at rest.';

COMMIT;
