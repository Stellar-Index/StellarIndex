-- 0097 down — restore the pre-widening CHECKs. Any rows using the new
-- event_kind values must be deleted first or the constraint re-add
-- fails (deliberate: down-migrating with data present should be loud,
-- not silent — same stance as 0092/0094/0095's down).
BEGIN;

DELETE FROM blend_emissions WHERE event_kind = 'update_emissions';
ALTER TABLE blend_emissions DROP CONSTRAINT blend_emissions_event_kind_check;
ALTER TABLE blend_emissions ADD CONSTRAINT blend_emissions_event_kind_check CHECK (event_kind IN (
    'gulp', 'claim',
    'reserve_emission_update', 'gulp_emissions',
    'bad_debt', 'defaulted_debt'
));

DELETE FROM blend_admin WHERE event_kind IN ('new_liquidation_auction', 'delete_liquidation_auction');
ALTER TABLE blend_admin DROP CONSTRAINT blend_admin_event_kind_check;
ALTER TABLE blend_admin ADD CONSTRAINT blend_admin_event_kind_check CHECK (event_kind IN (
    'set_admin', 'update_pool',
    'queue_set_reserve', 'cancel_set_reserve', 'set_reserve',
    'set_status', 'deploy'
));

COMMIT;
