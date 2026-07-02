-- 0070 up — admit `mint_and_forward` into cctp_events.event_type.
--
-- Board #31 follow-up: the CctpForwarder's fifth event decodes as of
-- v0.7.0, but migration 0038's CHECK enumerated only the original four
-- types, so every mint_and_forward insert was rejected (1,684 insert
-- errors on the first replay). Constraint recreated with the full set.
BEGIN;

ALTER TABLE cctp_events DROP CONSTRAINT cctp_events_event_type_check;
ALTER TABLE cctp_events ADD CONSTRAINT cctp_events_event_type_check CHECK (event_type IN (
    'deposit_for_burn',
    'mint_and_withdraw',
    'message_sent',
    'message_received',
    'mint_and_forward'
));

COMMIT;
