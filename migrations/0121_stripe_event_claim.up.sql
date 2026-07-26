-- 0121 up — a durable CLAIM on stripe_event_log, so two concurrent
-- deliveries of the same Stripe event cannot both be told to process it
-- (audit-2026-07-23 C3-039).
--
-- The race, precisely:
--
--   F-1322 made a row with processed_at IS NULL REPROCESSABLE. That was
--   the right call — a first delivery that failed mid-flight used to be
--   dup-acked forever, so a paying customer was silently never upgraded
--   — but it turned "the row exists" from a claim into a no-op. The
--   claim CTE now reads:
--
--     A: INSERT … ON CONFLICT DO NOTHING RETURNING → inserted=true → nil
--     B: insert no-ops, UNION arm reads processed_at = NULL     → nil
--
--   Both callers are told to proceed. processed_at is only stamped at
--   the END of the handler's work, so it cannot arbitrate the middle.
--   Stripe delivers at-least-once and its own retries are minutes apart,
--   so B does not even need to be simultaneous with A — it only needs to
--   arrive while A is still working.
--
-- `claimed_at` is that missing middle state: stamped when a delivery
-- takes the row, cleared when the attempt concludes without success
-- (MarkStripeEventFailed / MarkStripeEventDeadLettered), and superseded
-- by processed_at when it concludes WITH success.
--
-- Why a lease and not a boolean: a processor that is SIGKILLed leaves
-- its claim standing with nobody to release it. A claim older than the
-- lease is therefore re-claimable, which bounds the damage of a crash to
-- one lease period instead of forever. The lease lives in Go
-- (`postgresstore.stripeEventClaimLease`) rather than in a column
-- default because it is a property of how long the HANDLER can run, not
-- of the data.
--
-- This does NOT weaken F-1322: an unfinished row is still reprocessable.
-- It only makes "unfinished" mean "no live claim" instead of "processed_at
-- is NULL", which is what F-1322 meant in the first place.
--
-- Additive: one nullable column + one partial index, no existing column
-- touched, so the previous released binary (which never writes or reads
-- claimed_at) keeps working against this schema unchanged
-- (migrations/README.md rule 9). The pre-0121 binary simply reverts to
-- the pre-C3-039 behaviour for its own deliveries — it does not corrupt
-- anything a post-0121 binary wrote.

BEGIN;

ALTER TABLE stripe_event_log
    ADD COLUMN IF NOT EXISTS claimed_at timestamptz;

COMMENT ON COLUMN stripe_event_log.claimed_at IS
    'Set when a webhook delivery claims this event for processing; cleared '
    'when that attempt concludes without success (failed / dead-lettered) so '
    'the next retry re-attempts immediately. A claim older than the handler '
    'lease is re-claimable, which is what recovers a processor that died '
    'mid-flight. NULL means no delivery currently holds the row.';

-- Operator query: "which events are being worked on right now, and is
-- anything stuck holding a claim it will never release?" Partial over the
-- in-flight set only, which should normally be empty or tiny.
CREATE INDEX IF NOT EXISTS stripe_event_log_claimed_idx
    ON stripe_event_log (claimed_at)
    WHERE claimed_at IS NOT NULL AND processed_at IS NULL;

COMMIT;
