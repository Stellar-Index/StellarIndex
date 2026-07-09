-- 0094 up — admit the remaining 16 low-signal CCTP admin/governance
-- event types into cctp_events.event_type, completing the full topic
-- census.
--
-- ROADMAP #89c (2026-07-09): the #89b pass (migration 0092) added 5
-- governance events but docs/protocols/cctp.md flagged a "known gap"
-- of further lower-signal admin topics still undecoded. A full
-- topic_0_sym census against the ClickHouse raw lake (every event the
-- three CCTP contracts have EVER emitted — 26 distinct topics, 9496
-- total events, exactly reconciled; topics_xdr cross-checked for the
-- empty-topic_0_sym trap, none found) surfaced these 16:
--
--   admin_change_started           set_burn_limit_per_message
--   attester_enabled                set_token_controller
--   attester_manager_updated        signature_threshold_updated
--   denylisted                      swap_minter_config_set
--   denylister_changed              token_decimal_config_added
--   fee_recipient_set               un_denylisted
--   max_message_body_size_updated
--   min_fee_controller_set
--   pauser_changed
--   rescuer_changed
--
-- Same pattern as 0070/0092: DROP + re-ADD the CHECK with the full
-- set. Additive + old-binary-safe per rule 9 — the previous binary
-- never writes these event_type values, so widening what's ALLOWED
-- doesn't change its behavior; only the new binary emits rows using
-- the new values. This closes CLAUDE.md's "EVERY event for EVERY
-- Soroban protocol" gap for CCTP — docs/protocols/cctp.md's
-- known-gap note is retired in the same change.
BEGIN;

ALTER TABLE cctp_events DROP CONSTRAINT cctp_events_event_type_check;
ALTER TABLE cctp_events ADD CONSTRAINT cctp_events_event_type_check CHECK (event_type IN (
    'deposit_for_burn',
    'mint_and_withdraw',
    'message_sent',
    'message_received',
    'mint_and_forward',
    'ownership_transfer',
    'ownership_transfer_completed',
    'admin_changed',
    'remote_token_messenger_added',
    'token_pair_linked',
    'admin_change_started',
    'attester_enabled',
    'attester_manager_updated',
    'denylisted',
    'denylister_changed',
    'fee_recipient_set',
    'max_message_body_size_updated',
    'min_fee_controller_set',
    'pauser_changed',
    'rescuer_changed',
    'set_burn_limit_per_message',
    'set_token_controller',
    'signature_threshold_updated',
    'swap_minter_config_set',
    'token_decimal_config_added',
    'un_denylisted'
));

COMMIT;
