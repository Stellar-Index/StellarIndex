-- 0118 up — a durable dead-letter state on stripe_event_log, so
-- "customer paid, nothing provisioned" is a queryable, alertable row
-- instead of a WARN line nobody reads (audit-2026-07-23 C3-016).
--
-- The webhook has two terminal outcomes where money has landed at Stripe
-- and this system provisioned NOTHING:
--
--   1. `checkout.session.completed` for an identifier that holds no keys
--      ("customer paid but never signed up"). Pre-fix: one WARN,
--      processed_at stamped, 200. Once processed_at is set Stripe stops
--      retrying AND the handler's own dedupe dup-acks any manual
--      re-send — so the only recovery was a human noticing the log line.
--   2. Every per-key upgrade failing. Pre-fix worse: upgradeAllKeys
--      recorded the first error via MarkStripeEventFailed, and then the
--      handler's markStripeEventProcessed OVERWROTE it — that statement
--      is `SET processed_at = now(), error = NULL`. The failure erased
--      itself on the way out.
--
-- `error IS NOT NULL` cannot carry this state: it is set on transient,
-- still-retrying failures too, and it is cleared by the processed-mark.
-- What an operator needs is a distinct, sticky "this payment is
-- unreconciled" flag with an explicit close.
--
--   dead_lettered_at        when the handler concluded provisioning did
--                           not happen. Sticky: nothing clears it.
--   dead_letter_reason      the machine-readable why, so the alert can
--                           say which class fired
--                           (no_keys_for_identifier / key_upgrade_failed).
--   dead_letter_resolved_at stamped by MarkStripeEventProcessed when a
--                           later attempt finally provisions, so the
--                           open set self-clears on a successful retry
--                           and persists when one never comes.
--
-- The open set — and therefore the alert query — is:
--
--   SELECT * FROM stripe_event_log
--    WHERE dead_lettered_at IS NOT NULL
--      AND dead_letter_resolved_at IS NULL;
--
-- Deliberately NOT a separate table: the dedupe row already exists per
-- event, is already keyed by stripe_event_id, and already carries the
-- type + received_at + payload an operator needs to reconcile. A second
-- table would need its own write to stay in step with this one, which is
-- one more thing that can be half-written on the failure path.
--
-- Additive: three nullable columns + one partial index, no existing
-- column touched, so the previous released binary keeps working against
-- this schema unchanged (migrations/README.md rule 9).

BEGIN;

ALTER TABLE stripe_event_log
    ADD COLUMN IF NOT EXISTS dead_lettered_at        timestamptz,
    ADD COLUMN IF NOT EXISTS dead_letter_reason      text,
    ADD COLUMN IF NOT EXISTS dead_letter_resolved_at timestamptz;

COMMENT ON COLUMN stripe_event_log.dead_lettered_at IS
    'Set when a Stripe event was acknowledged but this system provisioned '
    'nothing for it (paid-with-no-keys, or every per-key upgrade failing). '
    'Sticky — the recovery is recorded in dead_letter_resolved_at, never by '
    'clearing this. NULL for every event that completed normally.';

COMMENT ON COLUMN stripe_event_log.dead_letter_reason IS
    'Machine-readable dead-letter class: no_keys_for_identifier | '
    'key_upgrade_failed. Distinct from `error`, which is also set on '
    'transient still-retrying failures and is cleared by the processed-mark.';

COMMENT ON COLUMN stripe_event_log.dead_letter_resolved_at IS
    'Set when a later delivery of a dead-lettered event finally provisioned. '
    'Rows with dead_lettered_at IS NOT NULL AND this NULL are the OPEN '
    'money-in-nothing-provisioned set an operator must reconcile.';

-- Partial index over the open set only: it should normally be empty, and
-- the alert/operator query is exactly this predicate.
CREATE INDEX IF NOT EXISTS stripe_event_log_open_dead_letters_idx
    ON stripe_event_log (dead_lettered_at)
    WHERE dead_lettered_at IS NOT NULL AND dead_letter_resolved_at IS NULL;

COMMIT;
