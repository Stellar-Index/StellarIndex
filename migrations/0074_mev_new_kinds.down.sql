-- 0074 down — revert the oracle_sandwich kind.
-- Deletes oracle_sandwich rows first so the narrower 0067 CHECK
-- applies (sandwich / liquidation_cascade / wash_trade rows were
-- already legal under 0067 and are kept).

BEGIN;

DELETE FROM mev_events WHERE kind = 'oracle_sandwich';

ALTER TABLE mev_events DROP CONSTRAINT IF EXISTS mev_events_kind_check;
ALTER TABLE mev_events ADD CONSTRAINT mev_events_kind_check
    CHECK (kind IN ('sandwich', 'oracle_deviation', 'liquidation_cascade',
                    'wash_trade', 'arbitrage'));

COMMIT;
