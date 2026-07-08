-- 0092 up — admit the 5 CCTP governance/admin event types into
-- cctp_events.event_type.
--
-- ROADMAP #89b (2026-07 decoder topic-match audit): the cctp source
-- was matching transfer-flow topics only (deposit_for_burn,
-- mint_and_withdraw, message_sent, message_received, mint_and_forward
-- — the last admitted by migration 0070). Five governance/admin
-- topics real mainnet events (verified against the ClickHouse raw
-- lake 2026-07-08 across all three CCTP contracts) were falling
-- through Classify() unhandled:
--
--   ownership_transfer            — 2-step ownership transfer initiated
--   ownership_transfer_completed  — 2-step ownership transfer accepted
--   admin_changed                 — admin role reassigned
--   remote_token_messenger_added  — TokenMessengerMinter only
--   token_pair_linked             — TokenMessengerMinter only
--
-- Same pattern as 0070: DROP + re-ADD the CHECK with the full set.
-- Additive + old-binary-safe per rule 9 — the previous binary never
-- writes these event_type values, so widening what's ALLOWED doesn't
-- change its behavior; only the new binary emits rows using the new
-- values.
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
    'token_pair_linked'
));

COMMIT;
