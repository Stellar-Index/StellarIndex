-- 0097 down — restore the pre-widening CHECKs. Any rows using the new
-- event_kind values must be deleted first or the constraint re-add
-- fails (deliberate: down-migrating with data present should be loud,
-- not silent — same stance as 0092/0094/0095's down).
BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM blend_emissions WHERE event_kind = 'update_emissions') THEN
    RAISE EXCEPTION '0097_blend_v1_pool_factory_events_check.down.sql: blend_emissions still holds rows where event_kind = ''update_emissions'' — down-migrating with data present is LOUD, not silent (#357). Delete them explicitly first if that is really what you want.';
  END IF;
END $$;
ALTER TABLE blend_emissions DROP CONSTRAINT blend_emissions_event_kind_check;
ALTER TABLE blend_emissions ADD CONSTRAINT blend_emissions_event_kind_check CHECK (event_kind IN (
    'gulp', 'claim',
    'reserve_emission_update', 'gulp_emissions',
    'bad_debt', 'defaulted_debt'
));

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM blend_admin WHERE event_kind IN ('new_liquidation_auction', 'delete_liquidation_auction')) THEN
    RAISE EXCEPTION '0097_blend_v1_pool_factory_events_check.down.sql: blend_admin still holds rows where event_kind IN (''new_liquidation_auction'', ''delete_liquidation_auction'') — down-migrating with data present is LOUD, not silent (#357). Delete them explicitly first if that is really what you want.';
  END IF;
END $$;
ALTER TABLE blend_admin DROP CONSTRAINT blend_admin_event_kind_check;
ALTER TABLE blend_admin ADD CONSTRAINT blend_admin_event_kind_check CHECK (event_kind IN (
    'set_admin', 'update_pool',
    'queue_set_reserve', 'cancel_set_reserve', 'set_reserve',
    'set_status', 'deploy'
));

COMMIT;
