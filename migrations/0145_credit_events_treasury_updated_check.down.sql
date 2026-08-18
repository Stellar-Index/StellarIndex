-- 0145 down — restore the four-value CHECK (pre-treasury_updated). Any
-- treasury_updated rows must be deleted first or the constraint re-add
-- fails (deliberate: down-migrating with data present should be loud, not
-- silent — same stance as 0095's down).
BEGIN;

DELETE FROM credit_events WHERE event_type = 'treasury_updated';
ALTER TABLE credit_events DROP CONSTRAINT credit_events_event_type_check;
ALTER TABLE credit_events ADD CONSTRAINT credit_events_event_type_check CHECK (event_type IN (
    'withdrawal', 'beacon_updated',
    'supported_asset_added', 'collateral_hash_updated'));

COMMIT;
