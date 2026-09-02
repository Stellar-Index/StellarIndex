-- 0070 down — restore the four-type CHECK. Any mint_and_forward rows
-- must be deleted EXPLICITLY first — this down now REFUSES if any exist (deliberate:
-- down-migrating with data present should be loud, not silent).
BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM cctp_events WHERE event_type = 'mint_and_forward') THEN
    RAISE EXCEPTION '0070_cctp_mint_and_forward_check.down.sql: cctp_events still holds rows where event_type = ''mint_and_forward'' — down-migrating with data present is LOUD, not silent (#357). Delete them explicitly first if that is really what you want.';
  END IF;
END $$;
ALTER TABLE cctp_events DROP CONSTRAINT cctp_events_event_type_check;
ALTER TABLE cctp_events ADD CONSTRAINT cctp_events_event_type_check CHECK (event_type IN (
    'deposit_for_burn',
    'mint_and_withdraw',
    'message_sent',
    'message_received'
));

COMMIT;
