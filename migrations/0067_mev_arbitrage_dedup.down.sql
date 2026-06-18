-- 0067 down — revert the arbitrage kind + dedup key.
-- Drops the dedup index/column and restores the original 4-kind CHECK.
-- Any 'arbitrage' rows are deleted first so the narrower CHECK applies.

BEGIN;

DELETE FROM mev_events WHERE kind = 'arbitrage';

DROP INDEX IF EXISTS mev_events_dedup_key_idx;
ALTER TABLE mev_events DROP COLUMN IF EXISTS dedup_key;

ALTER TABLE mev_events DROP CONSTRAINT IF EXISTS mev_events_kind_check;
ALTER TABLE mev_events ADD CONSTRAINT mev_events_kind_check
    CHECK (kind IN ('sandwich', 'oracle_deviation', 'liquidation_cascade',
                    'wash_trade'));

COMMIT;
