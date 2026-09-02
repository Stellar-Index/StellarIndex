-- 0095 down — restore the ten-kind CHECK (pre-rw_zone /
-- rw_zone_remove). Any rw_zone / rw_zone_remove rows must be deleted
-- EXPLICITLY first — this down now REFUSES if any exist (deliberate: down-migrating
-- with data present should be loud, not silent — same stance as
-- 0092/0094's down).
BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM blend_backstop_events WHERE event_kind IN ('rw_zone', 'rw_zone_remove')) THEN
    RAISE EXCEPTION '0095_blend_backstop_rw_zone_events_check.down.sql: blend_backstop_events still holds rows where event_kind IN (''rw_zone'', ''rw_zone_remove'') — down-migrating with data present is LOUD, not silent (#357). Delete them explicitly first if that is really what you want.';
  END IF;
END $$;
ALTER TABLE blend_backstop_events DROP CONSTRAINT blend_backstop_events_event_kind_check;
ALTER TABLE blend_backstop_events ADD CONSTRAINT blend_backstop_events_event_kind_check CHECK (event_kind IN (
    'deposit', 'claim', 'donate',
    'queue_withdrawal', 'withdraw', 'distribute',
    'gulp_emissions', 'dequeue_withdrawal', 'draw',
    'rw_zone_add'));

COMMIT;
