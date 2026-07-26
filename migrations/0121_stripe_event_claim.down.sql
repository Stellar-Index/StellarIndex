-- 0121 down — drop the Stripe event claim.
--
-- Less destructive than 0118's down (no reconciliation queue is erased —
-- an in-flight claim is transient state, not history), but it does
-- re-open the C3-039 race: without `claimed_at` two concurrent deliveries
-- of the same stripe_event_id are both told to proceed, because
-- processed_at is only stamped after the work completes and nothing else
-- records that a delivery is mid-flight.
--
-- Check nothing is in flight before reverting — a claim dropped
-- mid-handler is fine (the row stays unfinished and Stripe retries), but
-- it is worth knowing whether you are doing it under load:
--
--   SELECT stripe_event_id, type, claimed_at
--     FROM stripe_event_log
--    WHERE claimed_at IS NOT NULL AND processed_at IS NULL;
--
-- The post-0121 binary writes this column on every claim, so reverting
-- must be paired with a rollback to a pre-0121 binary
-- (migrations/README.md rule 9's two-release dance). Reverting the
-- migration alone leaves AppendStripeEvent referencing a column that no
-- longer exists, which fails EVERY webhook delivery closed — Stripe
-- retries, nothing is provisioned, and the dedupe surface is down.

BEGIN;

DROP INDEX IF EXISTS stripe_event_log_claimed_idx;

ALTER TABLE stripe_event_log
    DROP COLUMN IF EXISTS claimed_at;

COMMIT;
