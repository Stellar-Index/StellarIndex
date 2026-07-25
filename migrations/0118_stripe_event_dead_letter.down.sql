-- 0118 down — drop the Stripe dead-letter state.
--
-- Destructive in a way worth stating plainly: these columns are the ONLY
-- durable record of which paid Stripe events provisioned nothing. Every
-- other trace of that outcome is a log line (`error` is set on transient
-- failures too and is cleared by the processed-mark, which is why 0118
-- exists). Dropping them does not merely disable the alert — it erases
-- the open reconciliation queue, and a customer who paid and was never
-- provisioned becomes invisible again.
--
-- Export before reverting if any row is open:
--
--   SELECT stripe_event_id, type, received_at, dead_lettered_at,
--          dead_letter_reason
--     FROM stripe_event_log
--    WHERE dead_lettered_at IS NOT NULL
--      AND dead_letter_resolved_at IS NULL;
--
-- The post-0118 binary writes these columns on the webhook failure path,
-- so reverting must be paired with a rollback to a pre-0118 binary
-- (migrations/README.md rule 9's two-release dance). Reverting the
-- migration alone leaves the handler UPDATE-ing columns that no longer
-- exist, on the exact path that already means a customer paid for
-- nothing.

BEGIN;

DROP INDEX IF EXISTS stripe_event_log_open_dead_letters_idx;

ALTER TABLE stripe_event_log
    DROP COLUMN IF EXISTS dead_letter_resolved_at,
    DROP COLUMN IF EXISTS dead_letter_reason,
    DROP COLUMN IF EXISTS dead_lettered_at;

COMMIT;
