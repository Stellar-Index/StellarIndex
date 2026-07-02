-- 0070 down — restore the four-type CHECK. Any mint_and_forward rows
-- must be deleted first or the constraint re-add fails (deliberate:
-- down-migrating with data present should be loud, not silent).
BEGIN;

DELETE FROM cctp_events WHERE event_type = 'mint_and_forward';
ALTER TABLE cctp_events DROP CONSTRAINT cctp_events_event_type_check;
ALTER TABLE cctp_events ADD CONSTRAINT cctp_events_event_type_check CHECK (event_type IN (
    'deposit_for_burn',
    'mint_and_withdraw',
    'message_sent',
    'message_received'
));

COMMIT;
