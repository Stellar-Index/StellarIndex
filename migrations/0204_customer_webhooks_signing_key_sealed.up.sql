-- 0204 up — hold customer-webhook signing keys sealed at rest.
--
-- WHAT IS WRONG TODAY
--
-- customer_webhooks.secret_hash holds the RAW HMAC-SHA-256 signing key
-- (0151's comment says so), and previous_secret (0200) holds the key it
-- replaced at the last rotation, raw too. Neither can be one-way hashed —
-- the receiver verifies with the same shared secret — so anyone with
-- SELECT on the table, or a copy of a backup, can forge deliveries to
-- every endpoint.
--
-- WHAT THIS MIGRATION CHANGES
--
-- Adds signing_key_sealed: the key sealed by the API with AES-256-GCM
-- under a key that lives only in the API's environment, bound to the
-- row's id. secret_hash becomes nullable so a sealed row carries no raw
-- copy; a CHECK keeps at least one of the two present. The rotation
-- overlap key gets the same treatment in previous_signing_key_sealed,
-- and 0200's pair CHECK is widened to count either form as "set" while
-- allowing at most one. The API writes sealed-only keys when its seal
-- key is configured (create and rotation) and seals existing raw keys,
-- current and previous, at startup (compare-and-swap per key).
--
-- RULE 9. migrate up runs before the binary swap. The previous binary
-- names secret_hash with a value on INSERT (passes the CHECK), reads
-- only secret_hash and previous_secret, which are still populated for
-- every row until the new binary starts, and its rotation writes the raw
-- pair the widened CHECK still accepts. Rolling the BINARY back after it has sealed rows
-- leaves those rows unreadable to the old worker (it terminally fails
-- their deliveries as "no_secret"), so roll back the binary only
-- together with a restore of the raw keys.
--
-- Each ADD CONSTRAINT validates by scanning the table: one row per
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

ALTER TABLE customer_webhooks
    ADD COLUMN IF NOT EXISTS previous_signing_key_sealed bytea;

ALTER TABLE customer_webhooks
    DROP CONSTRAINT IF EXISTS customer_webhooks_previous_secret_pair;

ALTER TABLE customer_webhooks
    ADD CONSTRAINT customer_webhooks_previous_secret_pair  -- migration-compat:ok the old binary writes previous_secret and its expiry together and never previous_signing_key_sealed, which this check still accepts; every existing row satisfies 0200's narrower form
    CHECK ((previous_secret IS NULL OR previous_signing_key_sealed IS NULL)
           AND ((previous_secret IS NULL AND previous_signing_key_sealed IS NULL)
                = (previous_secret_expires_at IS NULL)));

COMMENT ON COLUMN customer_webhooks.signing_key_sealed IS
    'HMAC-SHA-256 signing key sealed by the API (version byte, then '
    'AES-256-GCM with the row id as associated data; key from the API '
    'env named by api.dashboard.webhook_seal_key_env). NULL only on a row '
    'still holding its key raw in secret_hash.';

COMMENT ON COLUMN customer_webhooks.previous_signing_key_sealed IS
    'The key signing_key_sealed (or secret_hash) held before the last '
    'rotation, sealed the same way with the same row id. Set only with '
    'previous_secret_expires_at; NULL while previous_secret holds it raw.';

COMMENT ON COLUMN customer_webhooks.previous_secret IS
    'The RAW HMAC key secret_hash held before the last rotation, for rows '
    'written without a seal key; NULL once sealed into '
    'previous_signing_key_sealed. Treat a non-NULL value as a credential '
    'at rest.';

COMMENT ON COLUMN customer_webhooks.secret_hash IS
    'MISNAMED legacy column. Holds the RAW HMAC-SHA-256 signing key, not a '
    'hash of one, for rows written without a seal key; NULL once the key '
    'is sealed into signing_key_sealed. Treat a non-NULL value as a '
    'credential at rest.';

COMMIT;
