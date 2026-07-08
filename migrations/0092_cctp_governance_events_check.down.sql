-- 0092 down — restore the five-type CHECK (pre-governance-events).
-- Any governance/admin rows must be deleted first or the constraint
-- re-add fails (deliberate: down-migrating with data present should
-- be loud, not silent — same stance as 0070's down).
BEGIN;

DELETE FROM cctp_events WHERE event_type IN (
    'ownership_transfer',
    'ownership_transfer_completed',
    'admin_changed',
    'remote_token_messenger_added',
    'token_pair_linked'
);
ALTER TABLE cctp_events DROP CONSTRAINT cctp_events_event_type_check;
ALTER TABLE cctp_events ADD CONSTRAINT cctp_events_event_type_check CHECK (event_type IN (
    'deposit_for_burn',
    'mint_and_withdraw',
    'message_sent',
    'message_received',
    'mint_and_forward'
));

COMMIT;
