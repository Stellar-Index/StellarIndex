-- 0235 up — customer_webhooks holds its current signing key in exactly one
-- form: raw (secret_hash) or sealed (signing_key_sealed), never both.
--
-- 0204's customer_webhooks_signing_key_present only required at least one.
-- Every reader prefers the sealed form, so a raw key written beside a sealed
-- one was ignored with no error, and SealLegacySigningKeys never repairs such
-- a row.
--
-- No repair: no binary since v0.99.0 writes such a row, so one is hand-written
-- and its raw key is as likely the intended one as the sealed key. A dual-key
-- row stops the migration with its id for an operator to resolve by hand
-- (pre-check: docs/operations/credential-rotation.md, "Migration pre-checks").
--
-- Compat: every binary since v0.99.0 writes the two columns exclusively
-- (CreateWebhook, RotateWebhookSecret and SealLegacySigningKeys each set one
-- and NULL the other), so v0.129.0 writes no row this rejects.
--
-- Lock: ADD CONSTRAINT and VALIDATE run in one transaction, so the ACCESS
-- EXCLUSIVE lock ADD takes is held until COMMIT; customer_webhooks is a
-- handful of rows per account, so that is a scan of milliseconds.

BEGIN;

SET LOCAL lock_timeout = '5s';

DO $$
DECLARE
    ids text;
BEGIN
    SELECT string_agg(id::text, ', ' ORDER BY id) INTO ids FROM customer_webhooks
     WHERE secret_hash IS NOT NULL AND signing_key_sealed IS NOT NULL;
    IF ids IS NOT NULL THEN
        RAISE EXCEPTION '0235: customer_webhooks rows hold both a raw and a sealed current key: %', ids
            USING HINT = 'Decide by hand which key each webhook should sign with, NULL the other column, then re-run.';
    END IF;
END $$;

ALTER TABLE customer_webhooks
    DROP CONSTRAINT IF EXISTS customer_webhooks_one_current_key;  -- migration-compat:ok drops only this migration's own constraint, for a re-run; no binary names it

ALTER TABLE customer_webhooks
    ADD CONSTRAINT customer_webhooks_one_current_key  -- migration-compat:ok v0.99.0+ CreateWebhook, RotateWebhookSecret and SealLegacySigningKeys always set one key column and NULL the other
    CHECK ((secret_hash IS NULL) <> (signing_key_sealed IS NULL)) NOT VALID;

ALTER TABLE customer_webhooks
    VALIDATE CONSTRAINT customer_webhooks_one_current_key;

COMMIT;
