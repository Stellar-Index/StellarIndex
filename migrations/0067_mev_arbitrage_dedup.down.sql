-- 0067 down — revert the arbitrage kind + dedup key.
-- Drops the dedup index/column and restores the original 4-kind CHECK.
-- Any 'arbitrage' rows must be deleted EXPLICITLY first — this down
-- REFUSES if any exist (down-migrating with data present is loud, not
-- silent).

BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM mev_events WHERE kind = 'arbitrage') THEN
    RAISE EXCEPTION '0067_mev_arbitrage_dedup.down.sql: mev_events still holds rows where kind = ''arbitrage'' — down-migrating with data present is LOUD, not silent (#357). Delete them explicitly first if that is really what you want.';
  END IF;
END $$;

DROP INDEX IF EXISTS mev_events_dedup_key_idx;
ALTER TABLE mev_events DROP COLUMN IF EXISTS dedup_key;

ALTER TABLE mev_events DROP CONSTRAINT IF EXISTS mev_events_kind_check;
ALTER TABLE mev_events ADD CONSTRAINT mev_events_kind_check
    CHECK (kind IN ('sandwich', 'oracle_deviation', 'liquidation_cascade',
                    'wash_trade'));

COMMIT;
