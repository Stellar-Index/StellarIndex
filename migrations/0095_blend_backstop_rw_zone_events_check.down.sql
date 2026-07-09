-- 0095 down — restore the ten-kind CHECK (pre-rw_zone /
-- rw_zone_remove). Any rw_zone / rw_zone_remove rows must be deleted
-- first or the constraint re-add fails (deliberate: down-migrating
-- with data present should be loud, not silent — same stance as
-- 0092/0094's down).
BEGIN;

DELETE FROM blend_backstop_events WHERE event_kind IN ('rw_zone', 'rw_zone_remove');
ALTER TABLE blend_backstop_events DROP CONSTRAINT blend_backstop_events_event_kind_check;
ALTER TABLE blend_backstop_events ADD CONSTRAINT blend_backstop_events_event_kind_check CHECK (event_kind IN (
    'deposit', 'claim', 'donate',
    'queue_withdrawal', 'withdraw', 'distribute',
    'gulp_emissions', 'dequeue_withdrawal', 'draw',
    'rw_zone_add'));

COMMIT;
