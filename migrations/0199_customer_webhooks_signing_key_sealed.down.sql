-- 0199 down — drop customer_webhooks.signing_key_sealed and restore
-- secret_hash NOT NULL with 0151's comment.
--
-- SQL cannot unseal a key, so this refuses while any row holds its key
-- only sealed: dropping the column would destroy that webhook's signing
-- key. Delete those webhooks (customers recreate them) or restore their
-- raw keys first.

BEGIN;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM customer_webhooks WHERE secret_hash IS NULL) THEN
        RAISE EXCEPTION '0199 down: customer_webhooks rows hold only a sealed signing key; '
                        'restore their raw keys or delete them before rolling back';
    END IF;
END
$$;

ALTER TABLE customer_webhooks
    DROP CONSTRAINT IF EXISTS customer_webhooks_signing_key_present;

ALTER TABLE customer_webhooks
    DROP COLUMN IF EXISTS signing_key_sealed;

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
