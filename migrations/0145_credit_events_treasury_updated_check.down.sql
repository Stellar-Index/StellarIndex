-- 0145 down — restore the four-value CHECK (pre-treasury_updated). Any
-- treasury_updated rows must be deleted first or the constraint re-add
-- fails (deliberate: down-migrating with data present should be loud, not
-- silent — same stance as 0095's down).
BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM credit_events WHERE event_type = 'treasury_updated') THEN
    RAISE EXCEPTION '0145_credit_events_treasury_updated_check.down.sql: credit_events still holds rows where event_type = ''treasury_updated'' — down-migrating with data present is LOUD, not silent (#357). Delete them explicitly first if that is really what you want.';
  END IF;
END $$;
ALTER TABLE credit_events DROP CONSTRAINT credit_events_event_type_check;
ALTER TABLE credit_events ADD CONSTRAINT credit_events_event_type_check CHECK (event_type IN (
    'withdrawal', 'beacon_updated',
    'supported_asset_added', 'collateral_hash_updated'));

COMMIT;
