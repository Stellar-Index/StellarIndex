-- 0204 down — drop customer_webhooks.signing_key_sealed and
-- previous_signing_key_sealed, restore secret_hash NOT NULL with 0151's
-- comment and 0200's previous_secret pair CHECK and comment.
--
-- SQL cannot unseal a key, so this refuses while any row holds its
-- current or previous key only sealed: dropping the column would destroy
-- that key. Delete those webhooks (customers recreate them) or restore their
-- raw keys first.

BEGIN;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM customer_webhooks WHERE secret_hash IS NULL) THEN
        RAISE EXCEPTION '0204 down: customer_webhooks rows hold only a sealed signing key; '
                        'restore their raw keys or delete them before rolling back';
    END IF;
    IF EXISTS (SELECT 1 FROM customer_webhooks WHERE previous_signing_key_sealed IS NOT NULL) THEN
        RAISE EXCEPTION '0204 down: customer_webhooks rows hold a sealed previous signing key; '
                        'restore their raw keys or delete them before rolling back';
    END IF;
END
$$;

ALTER TABLE customer_webhooks
    DROP CONSTRAINT IF EXISTS customer_webhooks_signing_key_present;

ALTER TABLE customer_webhooks
    DROP COLUMN IF EXISTS signing_key_sealed;

ALTER TABLE customer_webhooks
    DROP CONSTRAINT IF EXISTS customer_webhooks_previous_secret_pair;

ALTER TABLE customer_webhooks
    DROP COLUMN IF EXISTS previous_signing_key_sealed;

ALTER TABLE customer_webhooks
    ADD CONSTRAINT customer_webhooks_previous_secret_pair
    CHECK ((previous_secret IS NULL) = (previous_secret_expires_at IS NULL));

COMMENT ON COLUMN customer_webhooks.previous_secret IS
    'The RAW HMAC key secret_hash held before the last rotation (not a hash, like secret_hash). Deliveries are also signed with it until previous_secret_expires_at.';

ALTER TABLE customer_webhooks
    ALTER COLUMN secret_hash SET NOT NULL;

COMMENT ON COLUMN customer_webhooks.secret_hash IS
    'MISNAMED. Stores the RAW HMAC-SHA-256 signing key, not a hash of one — '
    'the receiver needs the shared secret to verify a delivery signature, so '
    'it cannot be one-way hashed (internal/platform/webhook.go). Treat this '
    'column as a credential at rest: it is NOT sealed the way '
    'accounts.mfa_secret_enc is. Renaming it to signing_key needs the rule-9 '
    'two-release dance (migrate up runs BEFORE the binary swap), so the name '
    'stays and this comment carries the truth (#346 F9).';

COMMIT;
