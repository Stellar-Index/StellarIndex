-- 0074 down — revert the oracle_sandwich kind.
-- Any oracle_sandwich rows must be deleted EXPLICITLY first — this down
-- REFUSES if any exist (down-migrating with data present is loud, not
-- silent). sandwich / liquidation_cascade / wash_trade / arbitrage rows
-- are legal under 0067's CHECK and are kept.

BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM mev_events WHERE kind = 'oracle_sandwich') THEN
    RAISE EXCEPTION '0074_mev_new_kinds.down.sql: mev_events still holds rows where kind = ''oracle_sandwich'' — down-migrating with data present is LOUD, not silent (#357). Delete them explicitly first if that is really what you want.';
  END IF;
END $$;

ALTER TABLE mev_events DROP CONSTRAINT IF EXISTS mev_events_kind_check;
ALTER TABLE mev_events ADD CONSTRAINT mev_events_kind_check
    CHECK (kind IN ('sandwich', 'oracle_deviation', 'liquidation_cascade',
                    'wash_trade', 'arbitrage'));

COMMIT;
