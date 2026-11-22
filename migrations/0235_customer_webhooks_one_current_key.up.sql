-- 0235 up — customer_webhooks holds its current signing key in exactly one
-- form: raw (secret_hash) or sealed (signing_key_sealed), never both.
--
-- 0204's customer_webhooks_signing_key_present only required at least one.
-- Every reader prefers the sealed form, so a raw key written beside a sealed
-- one (a hand-written restore UPDATE, or a pre-sealing binary's rotation) was
-- ignored with no error, and SealLegacySigningKeys never repairs such a row.
--
-- Repair first: a row holding both keeps the sealed key, the one every reader
-- already signs with, and drops the raw copy. Nothing is written in clear and
-- a re-run changes nothing.
--
-- Compat: every binary since v0.99.0 writes the two columns exclusively
-- (CreateWebhook, RotateWebhookSecret and SealLegacySigningKeys each set one
-- and NULL the other), so v0.129.0 writes no row this rejects.
--
-- Lock: customer_webhooks is a handful of rows per account. ADD CONSTRAINT
-- NOT VALID takes ACCESS EXCLUSIVE for a catalog change only; VALIDATE takes
-- SHARE UPDATE EXCLUSIVE and scans the table (milliseconds).

BEGIN;

SET LOCAL lock_timeout = '5s';

UPDATE customer_webhooks  -- migration-compat:ok clears only a raw copy no binary since v0.99.0 reads beside a sealed key
   SET secret_hash = NULL
 WHERE secret_hash IS NOT NULL
   AND signing_key_sealed IS NOT NULL;

DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM customer_webhooks
     WHERE secret_hash IS NOT NULL AND signing_key_sealed IS NOT NULL;
    IF n > 0 THEN
        RAISE EXCEPTION '0235: % customer_webhooks rows still hold both key forms after repair', n;
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
