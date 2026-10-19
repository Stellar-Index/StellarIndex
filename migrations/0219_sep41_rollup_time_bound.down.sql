-- 0219 down — drop the fold time bound; the delta read falls back to the
-- ledger-only filter.

BEGIN;

ALTER TABLE sep41_supply_rollup
    DROP COLUMN IF EXISTS bound_observed_at,
    DROP COLUMN IF EXISTS bound_ledger;

COMMIT;
